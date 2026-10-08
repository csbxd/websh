//go:build linux || darwin

package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxCommandBytes = 64 << 10
const maxHistory = 100
const maxHistoryBytes = 4 << 20

type command struct {
	ID         string     `json:"id"`
	Command    string     `json:"command"`
	Status     string     `json:"status"`
	Stdout     string     `json:"stdout"`
	Stderr     string     `json:"stderr"`
	ExitCode   *int       `json:"exit_code"`
	Cwd        string     `json:"cwd"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Truncated  bool       `json:"truncated"`
}

type sessionView struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Cwd             string    `json:"cwd"`
	State           string    `json:"state"`
	ActiveCommandID string    `json:"active_command_id,omitempty"`
	Commands        []command `json:"commands"`
}

type control struct{ id, phase, value, cwd string }
type run struct {
	command                    *command
	dir                        string
	out, err                   *os.File
	input                      *os.File
	ready, eof                 bool
	reason                     string
	stdoutBytes, stderrBytes   []byte
	outTruncated, errTruncated bool
}
type session struct {
	mu                   sync.Mutex
	id, name, cwd, state string
	limit                int
	proc                 *exec.Cmd
	script               io.WriteCloser
	controlRead          *os.File
	controls             chan control
	exited               chan struct{}
	exitCode             int
	reaped               bool
	active               *run
	history              []*command
	root                 string
	closeOnce            sync.Once
}

type manager struct {
	mu                 sync.Mutex
	shell, dir         string
	limit, maxSessions int
	sessions           map[string]*session
	closed             bool
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newManager(shell, dir string, outputLimit int) *manager {
	return &manager{shell: shell, dir: dir, limit: outputLimit, maxSessions: 16, sessions: make(map[string]*session)}
}

func (m *manager) create(name, cwd string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("server is shutting down")
	}
	live := 0
	for _, s := range m.sessions {
		if s.snapshot().State != "closed" {
			live++
		}
	}
	if live >= m.maxSessions {
		return nil, errors.New("session limit reached")
	}
	if len(m.sessions) >= m.maxSessions {
		for id, s := range m.sessions {
			if s.snapshot().State == "closed" {
				delete(m.sessions, id)
			}
		}
	}
	if cwd == "" {
		cwd = m.dir
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return nil, errors.New("working directory does not exist or is not a directory")
	}
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("shell-%d", len(m.sessions)+1)
	}
	if len(name) > 128 {
		return nil, errors.New("session name is too long")
	}
	s, err := startSession(m.shell, name, abs, m.limit)
	if err != nil {
		return nil, err
	}
	m.sessions[s.id] = s
	return s, nil
}

func (m *manager) get(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}
func (m *manager) list() []sessionView {
	m.mu.Lock()
	defer m.mu.Unlock()
	views := make([]sessionView, 0, len(m.sessions))
	for _, s := range m.sessions {
		views = append(views, s.snapshot())
	}
	return views
}
func (m *manager) close() {
	m.mu.Lock()
	m.closed = true
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
}

func startSession(shell, name, cwd string, limit int) (*session, error) {
	root, err := os.MkdirTemp("", "websh-session-")
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	proc := exec.Command(shell, "-s")
	proc.Dir = cwd
	proc.Env = append(os.Environ(), "TERM=dumb", "NO_COLOR=1", "CLICOLOR=0", "PWD="+cwd)
	proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	proc.ExtraFiles = []*os.File{w}
	proc.Stdout = io.Discard
	proc.Stderr = io.Discard
	stdin, err := proc.StdinPipe()
	if err != nil {
		r.Close()
		w.Close()
		os.RemoveAll(root)
		return nil, err
	}
	if err = proc.Start(); err != nil {
		stdin.Close()
		r.Close()
		w.Close()
		os.RemoveAll(root)
		return nil, err
	}
	w.Close()
	s := &session{id: randomID(), name: name, cwd: cwd, state: "idle", limit: limit, proc: proc, script: stdin, controlRead: r, controls: make(chan control, 32), exited: make(chan struct{}), root: root}
	// A caught SIGINT lets the evaluator survive while foreground children use
	// their default SIGINT disposition.
	go s.readControls()
	go func() {
		err := proc.Wait()
		code := 0
		if err != nil {
			code = 1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
				if code < 0 {
					if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
						code = 128 + int(ws.Signal())
					}
				}
			}
		}
		s.mu.Lock()
		s.exitCode = code
		s.state = "closed"
		// Clean up descendants immediately and never signal this PGID again.
		_ = syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
		s.reaped = true
		s.mu.Unlock()
		r.Close()
		stdin.Close()
		close(s.exited)
		s.mu.Lock()
		idle := s.active == nil
		s.mu.Unlock()
		if idle {
			os.RemoveAll(root)
		}
	}()
	if _, err = io.WriteString(stdin, "trap ':' INT\n"); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *session) readControls() {
	r := bufio.NewReaderSize(s.controlRead, 64<<10)
	for {
		parts := make([]string, 4)
		for i := range parts {
			p, err := r.ReadSlice(0)
			if err != nil {
				return
			}
			parts[i] = string(p[:len(p)-1])
		}
		select {
		case s.controls <- control{parts[0], parts[1], parts[2], parts[3]}:
		case <-s.exited:
			return
		}
	}
}

func (s *session) snapshot() sessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := sessionView{ID: s.id, Name: s.name, Cwd: s.cwd, State: s.state, Commands: make([]command, 0, len(s.history))}
	if s.active != nil {
		v.ActiveCommandID = s.active.command.ID
	}
	for _, c := range s.history {
		v.Commands = append(v.Commands, *c)
	}
	return v
}

func shellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'" }

func openFIFO(path string) (*os.File, error) {
	if err := syscall.Mkfifo(path, 0600); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

var errBusy = errors.New("session is running a command")
var errClosed = errors.New("session is closed")

func (s *session) execute(text string, timeout time.Duration) (command, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == "closed" {
		return command{}, errClosed
	}
	if s.active != nil {
		return command{}, errBusy
	}
	if strings.TrimSpace(text) == "" || len(text) > maxCommandBytes || strings.ContainsRune(text, 0) {
		return command{}, errors.New("command must be non-empty, contain no NUL, and be at most 64 KiB")
	}
	id := randomID()
	dir := filepath.Join(s.root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return command{}, err
	}
	fifo := filepath.Join(dir, "stdin")
	cleanup := func() { os.RemoveAll(dir) }
	input, err := openFIFO(fifo)
	if err != nil {
		cleanup()
		return command{}, err
	}
	out, err := openFIFO(filepath.Join(dir, "stdout"))
	if err != nil {
		input.Close()
		cleanup()
		return command{}, err
	}
	errout, err := openFIFO(filepath.Join(dir, "stderr"))
	if err != nil {
		input.Close()
		out.Close()
		cleanup()
		return command{}, err
	}
	c := &command{ID: id, Command: text, Status: "running", Cwd: s.cwd, StartedAt: time.Now().UTC()}
	run := &run{command: c, dir: dir, out: out, err: errout, input: input}
	s.active = run
	s.state = "running"
	if len(s.history) == maxHistory {
		s.history = s.history[1:]
	}
	s.history = append(s.history, c)
	// Redirect the group, not the individual eval, so syntax errors are captured.
	// The script channel and command stdin are independent, including heredocs.
	script := fmt.Sprintf("{ command printf '%%s\\0%%s\\0%%s\\0%%s\\0' %s ready 0 '' >&3; (exit \"${__websh_rc:-0}\"); eval %s; } <%s >%s 2>%s\n__websh_rc=$?\ncommand printf '%%s\\0%%s\\0%%s\\0%%s\\0' %s done \"$__websh_rc\" \"$(command pwd -P)\" >&3\n", shellQuote(id), shellQuote(text), shellQuote(fifo), shellQuote(out.Name()), shellQuote(errout.Name()), shellQuote(id))
	go s.monitor(run, timeout)
	if _, err := io.WriteString(s.script, script); err != nil {
		run.reason = "failed"
		_ = syscall.Kill(-s.proc.Process.Pid, syscall.SIGKILL)
	}
	return *c, nil
}

// Output FIFOs give writers backpressure without accumulating capture files on
// disk or restricting ordinary files that a command creates. Bound each drain
// so a continuous writer cannot starve stderr, control records or cancellation.
func drainOutput(f *os.File, data *[]byte, limit, budget int) (changed, truncated bool) {
	var buf [16384]byte
	for budget > 0 {
		n, err := syscall.Read(int(f.Fd()), buf[:min(len(buf), budget)])
		if err == syscall.EINTR {
			continue
		}
		if n > 0 {
			keep := min(n, limit-len(*data))
			if keep > 0 {
				*data = append(*data, buf[:keep]...)
				changed = true
			}
			if keep < n {
				truncated = true
			}
			budget -= n
		}
		if err != nil || n == 0 {
			break
		}
	}
	return
}

func (s *session) capture(r *run, budget int) bool {
	changedOut, truncatedOut := drainOutput(r.out, &r.stdoutBytes, s.limit, budget)
	changedErr, truncatedErr := drainOutput(r.err, &r.stderrBytes, s.limit, budget)
	r.outTruncated = r.outTruncated || truncatedOut
	r.errTruncated = r.errTruncated || truncatedErr
	s.mu.Lock()
	if changedOut {
		r.command.Stdout = strings.ToValidUTF8(string(r.stdoutBytes), "�")
	}
	if changedErr {
		r.command.Stderr = strings.ToValidUTF8(string(r.stderrBytes), "�")
	}
	r.command.Truncated = r.outTruncated || r.errTruncated
	s.mu.Unlock()
	return r.outTruncated || r.errTruncated
}

func (s *session) monitor(r *run, timeout time.Duration) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var timer *time.Timer
	var deadline <-chan time.Time
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		deadline = timer.C
		defer timer.Stop()
	}
	code, cwd := 1, ""
	for {
		select {
		case message := <-s.controls:
			if message.id != r.command.ID {
				continue
			}
			if message.phase == "ready" {
				s.mu.Lock()
				r.ready = true
				if r.eof && r.input != nil {
					r.input.Close()
					r.input = nil
				}
				s.mu.Unlock()
			}
			if message.phase == "done" {
				code, _ = strconv.Atoi(message.value)
				cwd = message.cwd
				goto finished
			}
		case <-s.exited:
			// Process exit can race the final control record: consume queued done
			// records before interpreting an early shell termination.
			select {
			case message := <-s.controls:
				if message.id == r.command.ID && message.phase == "done" {
					code, _ = strconv.Atoi(message.value)
					cwd = message.cwd
					goto finished
				}
			default:
			}
			s.mu.Lock()
			code = s.exitCode
			s.mu.Unlock()
			goto finished
		case <-deadline:
			s.interruptRun(r, "timed_out")
			deadline = nil
		case <-ticker.C:
			if s.capture(r, 128<<10) {
				s.interruptRun(r, "interrupted")
			}
		}
	}
finished:
	// Foreground writes precede the done record; drain remaining pipe bytes.
	s.capture(r, s.limit+(1<<20))
	s.mu.Lock()
	if r.input != nil {
		r.input.Close()
		r.input = nil
	}
	status := "completed"
	if r.reason != "" {
		status = r.reason
	}
	if r.command.Truncated && r.reason == "" {
		status = "interrupted"
	}
	if s.state == "closed" && code == 0 && r.reason == "" && cwd == "" {
		status = "completed"
	}
	r.command.Status = status
	r.command.ExitCode = &code
	finishedAt := time.Now().UTC()
	r.command.FinishedAt = &finishedAt
	if cwd != "" {
		s.cwd = cwd
		r.command.Cwd = cwd
	}
	s.active = nil
	// Keep recent transcript text bounded as well as individual output streams.
	bytes := 0
	for _, c := range s.history {
		bytes += len(c.Command) + len(c.Stdout) + len(c.Stderr)
	}
	for len(s.history) > 1 && bytes > maxHistoryBytes {
		old := s.history[0]
		bytes -= len(old.Command) + len(old.Stdout) + len(old.Stderr)
		s.history = s.history[1:]
	}
	closed := s.state == "closed"
	if !closed {
		s.state = "idle"
	}
	s.mu.Unlock()
	r.out.Close()
	r.err.Close()
	os.RemoveAll(r.dir)
	if closed {
		os.RemoveAll(s.root)
	}
}

func (s *session) interruptRun(r *run, reason string) {
	s.mu.Lock()
	if s.active != r || r.reason != "" || s.reaped || s.state == "closed" {
		s.mu.Unlock()
		return
	}
	r.reason = reason
	_ = syscall.Kill(-s.proc.Process.Pid, syscall.SIGINT)
	s.mu.Unlock()
	time.AfterFunc(750*time.Millisecond, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.active == r && !s.reaped {
			_ = syscall.Kill(-s.proc.Process.Pid, syscall.SIGKILL)
		}
	})
}

func (s *session) interrupt() error {
	s.mu.Lock()
	r := s.active
	s.mu.Unlock()
	if r == nil {
		return errors.New("no running command")
	}
	s.interruptRun(r, "interrupted")
	return nil
}

func (s *session) sendInput(data string, eof bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.active
	if r == nil {
		return errors.New("no running command")
	}
	if r.input == nil || r.eof {
		return errors.New("stdin is closed")
	}
	if len(data) > 4096 {
		return errors.New("input must be at most 4096 bytes per request")
	}
	if len(data) > 0 {
		n, err := syscall.Write(int(r.input.Fd()), []byte(data))
		if err != nil {
			return errors.New("stdin buffer is full; retry after the command reads input")
		}
		if n != len(data) {
			return errors.New("stdin accepted only part of the input")
		}
	}
	if eof {
		r.eof = true
		if r.ready {
			r.input.Close()
			r.input = nil
		}
	}
	return nil
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.reaped {
			s.mu.Unlock()
			return
		}
		s.state = "closed"
		if s.active != nil && s.active.reason == "" {
			s.active.reason = "interrupted"
		}
		_ = syscall.Kill(-s.proc.Process.Pid, syscall.SIGKILL)
		s.mu.Unlock()
		select {
		case <-s.exited:
		case <-time.After(2 * time.Second):
		}
	})
}
