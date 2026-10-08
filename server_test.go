//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testCommand struct {
	ID         string `json:"id"`
	Command    string `json:"command"`
	Status     string `json:"status"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   *int   `json:"exit_code"`
	CWD        string `json:"cwd"`
	FinishedAt string `json:"finished_at"`
	Truncated  bool   `json:"truncated"`
}

type testSession struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	CWD             string        `json:"cwd"`
	State           string        `json:"state"`
	ActiveCommandID string        `json:"active_command_id"`
	Commands        []testCommand `json:"commands"`
}

type testAPI struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	dir    string
}

func newTestAPI(t *testing.T, outputLimit int) *testAPI {
	t.Helper()
	return newTestAPIWithShell(t, outputLimit, "/bin/sh")
}

func newTestAPIWithShell(t *testing.T, outputLimit int, shell string) *testAPI {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(shell, dir, outputLimit)
	t.Cleanup(m.close)
	srv := httptest.NewServer(newHandler(m, "integration-test-token"))
	t.Cleanup(srv.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &testAPI{t: t, server: srv, client: &http.Client{Jar: jar, Timeout: 3 * time.Second}, dir: dir}
	status := a.request(http.MethodPost, "/api/login", map[string]any{"token": "integration-test-token"}, nil)
	if status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("login status = %d", status)
	}
	return a
}

func (a *testAPI) request(method, path string, body, result any) int {
	a.t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, a.server.URL+path, bytes.NewReader(data))
	if err != nil {
		a.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			a.t.Fatalf("%s %s: decode status %d: %v", method, path, resp.StatusCode, err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode
}

func (a *testAPI) create(name string) testSession {
	a.t.Helper()
	var s testSession
	status := a.request(http.MethodPost, "/api/sessions", map[string]any{"name": name, "cwd": a.dir}, &s)
	if status < 200 || status >= 300 || s.ID == "" {
		a.t.Fatalf("create session: status=%d, session=%+v", status, s)
	}
	return s
}

func (a *testAPI) snapshot(id string) testSession {
	a.t.Helper()
	var s testSession
	if status := a.request(http.MethodGet, "/api/sessions/"+id, nil, &s); status != http.StatusOK {
		a.t.Fatalf("session snapshot status = %d", status)
	}
	return s
}

func (a *testAPI) start(id, command string, timeoutSeconds int) testCommand {
	a.t.Helper()
	var c testCommand
	body := map[string]any{"command": command}
	if timeoutSeconds != 0 {
		body["timeout_seconds"] = timeoutSeconds
	}
	status := a.request(http.MethodPost, "/api/sessions/"+id+"/commands", body, &c)
	if status < 200 || status >= 300 || c.ID == "" {
		a.t.Fatalf("start command: status=%d, command=%+v", status, c)
	}
	return c
}

func (a *testAPI) waitCommand(sessionID, commandID string, predicate func(testCommand) bool) testCommand {
	a.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last testCommand
	for time.Now().Before(deadline) {
		for _, c := range a.snapshot(sessionID).Commands {
			if c.ID == commandID {
				last = c
				if predicate(c) {
					return c
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.t.Fatalf("command %s did not reach expected state: %+v", commandID, last)
	return last
}

func finished(c testCommand) bool {
	return c.FinishedAt != "" || c.Status == "completed" || c.Status == "interrupted" || c.Status == "timed_out" || c.Status == "failed"
}

func (a *testAPI) run(id, command string) testCommand {
	a.t.Helper()
	c := a.start(id, command, 0)
	return a.waitCommand(id, c.ID, finished)
}

func assertExit(t *testing.T, c testCommand, code int) {
	t.Helper()
	if c.ExitCode == nil || *c.ExitCode != code {
		t.Fatalf("exit code: want %d, command=%+v", code, c)
	}
}

func TestPersistentShellAndStructuredOutput(t *testing.T) {
	a := newTestAPI(t, 1<<20)
	if err := os.Mkdir(filepath.Join(a.dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	s := a.create("persistent")
	c := a.run(s.ID, "cd sub\nexport WEBSH_TEST_VALUE='persisted value'\nwebsh_fn() { printf 'function:%s\\n' \"$WEBSH_TEST_VALUE\"; }\nprintf 'set\\n'")
	assertExit(t, c, 0)
	if c.Stdout != "set\n" || c.Stderr != "" {
		t.Fatalf("setup output = %+v", c)
	}
	c = a.run(s.ID, "printf 'env:%s\\n' \"$WEBSH_TEST_VALUE\"; websh_fn; pwd")
	assertExit(t, c, 0)
	wantCWD := filepath.Join(a.dir, "sub")
	want := "env:persisted value\nfunction:persisted value\n" + wantCWD + "\n"
	if c.Stdout != want || c.CWD != wantCWD || a.snapshot(s.ID).CWD != wantCWD {
		t.Fatalf("persistent state: want stdout=%q cwd=%q; command=%+v", want, wantCWD, c)
	}
	c = a.run(s.ID, "cat <<'WEBSH_DOC'\nline one\nline $two\nWEBSH_DOC")
	assertExit(t, c, 0)
	if c.Stdout != "line one\nline $two\n" {
		t.Fatalf("heredoc output = %q", c.Stdout)
	}
	c = a.run(s.ID, "printf 'output'; printf 'error' >&2; (exit 7)")
	assertExit(t, c, 7)
	if c.Stdout != "output" || c.Stderr != "error" || c.Status != "completed" {
		t.Fatalf("stdout/stderr/nonzero result = %+v", c)
	}
	c = a.run(s.ID, "command printf '%s' \"$?\"")
	assertExit(t, c, 0)
	if c.Stdout != "7" {
		t.Fatalf("last exit status should persist across submissions: %q", c.Stdout)
	}
	c = a.run(s.ID, "printf '\\033[31mred\\033[0m\\n'")
	assertExit(t, c, 0)
	if c.Stdout != "\x1b[31mred\x1b[0m\n" {
		t.Fatalf("output text should retain its control bytes: %q", c.Stdout)
	}
	c = a.run(s.ID, "printf() { command printf 'custom:%s\\n' \"$1\"; }; unset PWD; command printf 'real\\n'; command pwd -P")
	assertExit(t, c, 0)
	if c.Stdout != "real\n"+wantCWD+"\n" || c.CWD != wantCWD {
		t.Fatalf("shadowed printf/unset PWD broke protocol or cwd: %+v", c)
	}
	c = a.run(s.ID, "printf payload")
	assertExit(t, c, 0)
	if c.Stdout != "custom:payload\n" || a.snapshot(s.ID).State != "idle" {
		t.Fatalf("user printf function should persist without corrupting control records: %+v", c)
	}
	c = a.run(s.ID, "unset PWD; set -u; command printf 'nounset\\n'")
	assertExit(t, c, 0)
	c = a.run(s.ID, "command printf 'after-nounset\\n'")
	assertExit(t, c, 0)
	if c.Stdout != "after-nounset\n" || c.CWD != wantCWD || a.snapshot(s.ID).State != "idle" {
		t.Fatalf("unset PWD with nounset should preserve protocol and cwd: %+v", c)
	}
}

func TestStreamingInputAndBusySession(t *testing.T) {
	a := newTestAPI(t, 1<<20)
	s := a.create("input")
	c := a.start(s.ID, "printf 'ready\\n'; read answer; printf 'answer:%s\\n' \"$answer\"", 0)
	c = a.waitCommand(s.ID, c.ID, func(c testCommand) bool { return strings.Contains(c.Stdout, "ready\n") })
	if finished(c) || c.Status != "running" {
		t.Fatalf("read should be running while output is visible: %+v", c)
	}
	if status := a.request(http.MethodPost, "/api/sessions/"+s.ID+"/commands", map[string]any{"command": "printf should-not-run"}, nil); status != http.StatusConflict {
		t.Fatalf("busy session status = %d, want 409", status)
	}
	if status := a.request(http.MethodPost, "/api/sessions/"+s.ID+"/input", map[string]any{"data": "hello\n"}, nil); status < 200 || status >= 300 {
		t.Fatalf("input status = %d", status)
	}
	c = a.waitCommand(s.ID, c.ID, finished)
	assertExit(t, c, 0)
	if c.Stdout != "ready\nanswer:hello\n" {
		t.Fatalf("interactive output = %q", c.Stdout)
	}
	c = a.start(s.ID, "printf 'cat-ready\\n'; cat", 0)
	a.waitCommand(s.ID, c.ID, func(c testCommand) bool { return strings.Contains(c.Stdout, "cat-ready\n") })
	if status := a.request(http.MethodPost, "/api/sessions/"+s.ID+"/input", map[string]any{"data": "from input\n", "eof": true}, nil); status < 200 || status >= 300 {
		t.Fatalf("input+EOF status = %d", status)
	}
	c = a.waitCommand(s.ID, c.ID, finished)
	assertExit(t, c, 0)
	if c.Stdout != "cat-ready\nfrom input\n" {
		t.Fatalf("EOF output = %q", c.Stdout)
	}
	assertExit(t, a.run(s.ID, "printf 'after-eof'"), 0)
}

func TestImmediateInputEOF(t *testing.T) {
	shell := filepath.Join(t.TempDir(), "delayed-sh")
	// Hold the evaluator's startup so the input request arrives before it opens
	// the command FIFO, making the EOF-before-readiness regression deterministic.
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nsleep 0.2\nexec /bin/sh \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := newTestAPIWithShell(t, 1<<20, shell)
	s := a.create("immediate-eof")
	// Deliver stdin as soon as command submission returns, without waiting for
	// output or shell readiness. EOF must not prevent opening the command FIFO.
	c := a.start(s.ID, "cat", 0)
	if status := a.request(http.MethodPost, "/api/sessions/"+s.ID+"/input", map[string]any{"data": "early input\n", "eof": true}, nil); status < 200 || status >= 300 {
		t.Fatalf("immediate EOF status = %d", status)
	}
	c = a.waitCommand(s.ID, c.ID, finished)
	assertExit(t, c, 0)
	if c.Stdout != "early input\n" {
		t.Fatalf("immediate EOF output = %q", c.Stdout)
	}
}

func TestInterruptTimeoutAndShellExit(t *testing.T) {
	a := newTestAPI(t, 1<<20)
	for _, tc := range []struct {
		name    string
		timeout int
		status  string
	}{
		{name: "interrupt", status: "interrupted"},
		{name: "timeout", timeout: 1, status: "timed_out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := a.create(tc.name)
			c := a.start(s.ID, "printf 'started\\n'; sleep 30", tc.timeout)
			a.waitCommand(s.ID, c.ID, func(c testCommand) bool { return strings.Contains(c.Stdout, "started\n") })
			if tc.timeout == 0 {
				if status := a.request(http.MethodPost, "/api/sessions/"+s.ID+"/interrupt", map[string]any{}, nil); status < 200 || status >= 300 {
					t.Fatalf("interrupt status = %d", status)
				}
			}
			c = a.waitCommand(s.ID, c.ID, finished)
			if c.Status != tc.status {
				t.Fatalf("command status = %q, want %q", c.Status, tc.status)
			}
			assertExit(t, a.run(s.ID, "printf 'still-alive'"), 0)
		})
	}
	s := a.create("exit")
	c := a.run(s.ID, "printf 'bye\\n'; exit 7")
	assertExit(t, c, 7)
	if c.Stdout != "bye\n" || a.snapshot(s.ID).State != "closed" {
		t.Fatalf("shell exit result = %+v, session = %+v", c, a.snapshot(s.ID))
	}
	if status := a.request(http.MethodPost, "/api/sessions/"+s.ID+"/commands", map[string]any{"command": "true"}, nil); status < 400 {
		t.Fatalf("closed shell accepted command: status = %d", status)
	}
}

func TestInterruptKillsShellThatIgnoresSignal(t *testing.T) {
	a := newTestAPI(t, 1<<20)
	s := a.create("ignore-interrupt")
	c := a.start(s.ID, "trap '' INT; printf 'ready\\n'; while :; do :; done", 0)
	a.waitCommand(s.ID, c.ID, func(c testCommand) bool { return strings.Contains(c.Stdout, "ready\n") })
	if status := a.request(http.MethodPost, "/api/sessions/"+s.ID+"/interrupt", map[string]any{}, nil); status < 200 || status >= 300 {
		t.Fatalf("interrupt status = %d", status)
	}
	c = a.waitCommand(s.ID, c.ID, finished)
	if c.Status != "interrupted" || a.snapshot(s.ID).State != "closed" {
		t.Fatalf("ignored interrupt must terminate session: command=%+v session=%+v", c, a.snapshot(s.ID))
	}
}

func TestMalformedCommandFinalizesAndCapturesError(t *testing.T) {
	a := newTestAPI(t, 1<<20)
	s := a.create("syntax-error")
	c := a.run(s.ID, "if")
	if c.ExitCode == nil || *c.ExitCode == 0 || c.Stderr == "" {
		t.Fatalf("malformed command did not retain a nonzero exit and stderr: %+v", c)
	}
	if a.snapshot(s.ID).State != "closed" {
		t.Fatalf("noninteractive sh syntax error must close the shell: %+v", a.snapshot(s.ID))
	}
}

func TestSetEFailureClosesSession(t *testing.T) {
	a := newTestAPI(t, 1<<20)
	s := a.create("errexit")
	c := a.run(s.ID, "set -e; printf 'before\\n'; false; printf 'unreachable\\n'")
	assertExit(t, c, 1)
	if c.Stdout != "before\n" || a.snapshot(s.ID).State != "closed" {
		t.Fatalf("set -e failure result = %+v, session = %+v", c, a.snapshot(s.ID))
	}
}

func TestOutputLimitStopsRunawayCommand(t *testing.T) {
	const limit = 256
	a := newTestAPI(t, limit)
	s := a.create("bounded-output")
	c := a.run(s.ID, "while :; do printf '0123456789abcdef'; done")
	if !c.Truncated || c.Status != "interrupted" || len(c.Stdout) > limit || len(c.Stdout) == 0 {
		t.Fatalf("bounded output result = %+v (bytes=%d)", c, len(c.Stdout))
	}
}

func TestShellKeepsOriginalFileSizeLimit(t *testing.T) {
	want, err := exec.Command("/bin/sh", "-c", "ulimit -f").Output()
	if err != nil {
		t.Fatal(err)
	}
	a := newTestAPI(t, 1<<20)
	s := a.create("file-limit")
	c := a.run(s.ID, "ulimit -f")
	assertExit(t, c, 0)
	if c.Stdout != string(want) {
		t.Fatalf("output capture must not alter the shell's file-size limit: want %q, got %q", want, c.Stdout)
	}
}

func TestLargeOutputDrainsBothStreams(t *testing.T) {
	a := newTestAPI(t, 256<<10)
	s := a.create("large-output")
	c := a.run(s.ID, "i=0; while [ \"$i\" -lt 10000 ]; do printf '0123456789abcdef'; printf 'fedcba9876543210' >&2; i=$((i+1)); done")
	assertExit(t, c, 0)
	if c.Status != "completed" || c.Truncated {
		t.Fatalf("large output below cap should complete: %+v", c)
	}
	// Each stream is 160,000 bytes, larger than ordinary kernel pipe buffers. Both
	// must drain concurrently without blocking the shell or dropping bytes.
	if c.Stdout != strings.Repeat("0123456789abcdef", 10000) || c.Stderr != strings.Repeat("fedcba9876543210", 10000) {
		t.Fatalf("large output mismatch: stdout bytes=%d stderr bytes=%d", len(c.Stdout), len(c.Stderr))
	}
}

func TestSessionListAndDelete(t *testing.T) {
	a := newTestAPI(t, 1<<20)
	s := a.create("delete-me")
	var list struct {
		Sessions []testSession `json:"sessions"`
	}
	if status := a.request(http.MethodGet, "/api/sessions", nil, &list); status != http.StatusOK || len(list.Sessions) != 1 || list.Sessions[0].ID != s.ID {
		t.Fatalf("sessions list: status=%d sessions=%+v", status, list.Sessions)
	}
	if status := a.request(http.MethodDelete, "/api/sessions/"+s.ID, nil, nil); status < 200 || status >= 300 {
		t.Fatalf("delete status = %d", status)
	}
	if closed := a.snapshot(s.ID); closed.State != "closed" {
		t.Fatalf("deleted session should retain closed snapshot: %+v", closed)
	}
}

func TestAuthenticationAndCrossOriginProtection(t *testing.T) {
	a := newTestAPI(t, 1<<20)
	client := &http.Client{Timeout: 3 * time.Second}
	for _, path := range []string{"/api/info", "/api/sessions"} {
		resp, err := client.Get(a.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		want := http.StatusUnauthorized
		if path == "/api/info" {
			want = http.StatusOK
		}
		if resp.StatusCode != want {
			t.Fatalf("unauthenticated %s status=%d want=%d", path, resp.StatusCode, want)
		}
	}
	resp, err := client.Post(a.server.URL+"/api/login", "application/json", strings.NewReader(`{"token":"incorrect"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("incorrect token status = %d", resp.StatusCode)
	}
	resp, err = client.Post(a.server.URL+"/api/login", "application/json", strings.NewReader(`{"token":"integration-test-token"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	cookies := resp.Cookies()
	if len(cookies) == 0 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("login cookie protections = %+v", cookies)
	}
	req, err := http.NewRequest(http.MethodPost, a.server.URL+"/api/sessions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://untrusted.example")
	resp, err = a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin request status = %d, want 403", resp.StatusCode)
	}
	for _, tc := range []struct {
		name   string
		header string
		value  string
		want   int
	}{
		{name: "valid bearer", header: "Authorization", value: "Bearer integration-test-token", want: http.StatusOK},
		{name: "invalid bearer", header: "Authorization", value: "Bearer incorrect", want: http.StatusUnauthorized},
		{name: "cross-site mutation without origin", header: "Sec-Fetch-Site", value: "cross-site", want: http.StatusForbidden},
		{name: "unrecognized host", header: "Host", value: "untrusted.example", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := http.MethodGet
			if tc.header == "Sec-Fetch-Site" {
				method = http.MethodPost
			}
			req, err := http.NewRequest(method, a.server.URL+"/api/sessions", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.header == "Host" {
				req.Host = tc.value
			} else {
				req.Header.Set(tc.header, tc.value)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d", resp.StatusCode, tc.want)
			}
		})
	}
}
