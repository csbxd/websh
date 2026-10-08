//go:build linux || darwin

package websh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type identityKey struct{}

func libraryOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Shell: "/bin/sh", Dir: t.TempDir(), OutputLimit: 1 << 20, MaxSessions: 16,
		Token: "library-test-token",
	}
}

func externalOptions(t *testing.T) Options {
	options := libraryOptions(t)
	options.LoginURL, options.LogoutURL = "/", "/_auth/logout"
	options.Authenticate = func(r *http.Request) bool {
		return r.Context().Value(identityKey{}) == "alice"
	}
	return options
}

func libraryRequest(handler http.Handler, method, target, origin, credential string, authenticated bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	if credential == "bearer" {
		r.Header.Set("Authorization", "Bearer library-test-token")
	}
	if credential == "cookie" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: "library-test-token"})
	}
	if credential == "headers" {
		r.Header.Set("Remote-User", "alice")
		r.Header.Set("X-Auth-Request-User", "alice")
	}
	if authenticated {
		r = r.WithContext(context.WithValue(r.Context(), identityKey{}, "alice"))
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestExternalAuthenticationRequiresCallbackOnEveryRoute(t *testing.T) {
	t.Setenv("WEBSH_TOKEN", "library-test-token")
	handler, close, err := New(externalOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(close)
	a := handler.(*apiServer)
	if a.token != "" {
		t.Fatal("external authentication retained the native token")
	}
	a.token = "library-test-token" // Even a stale native token cannot bypass the callback.
	for _, path := range []string{"/", "/index.html", "/app.js", "/style.css", "/api/info", "/api/sessions", "/api/login", "/missing"} {
		for _, credential := range []string{"", "bearer", "cookie", "headers"} {
			w := libraryRequest(a, http.MethodGet, "https://localhost"+path, "", credential, false)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s with %s: HTTP %d", path, credential, w.Code)
			}
		}
	}
	for _, method := range []string{"GET", "POST", "PUT", "HEAD"} {
		w := libraryRequest(a, method, "https://shell.example.test/api/login", "https://shell.example.test", "bearer", true)
		if w.Code != http.StatusNotFound || len(w.Result().Cookies()) != 0 {
			t.Errorf("native %s login remained enabled: %d", method, w.Code)
		}
	}
	w := libraryRequest(a, http.MethodGet, "https://shell.example.test/api/info", "", "", true)
	var info struct{ Authenticated bool }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &info) != nil || !info.Authenticated {
		t.Fatalf("callback identity rejected: %d %s", w.Code, w.Body.String())
	}
}

func TestExternalAuthenticationPreservesHostAndOriginChecks(t *testing.T) {
	handler, close, err := New(externalOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(close)
	if w := libraryRequest(handler, "GET", "https://shell.example.test/api/info", "", "", false); w.Code != 403 {
		t.Fatalf("unauthenticated custom Host accepted: %d", w.Code)
	}
	for _, origin := range []string{"https://evil.test", "http://shell.example.test", "https://user@shell.example.test", "https://shell.example.test/path", "https://shell.example.test?query=1"} {
		w := libraryRequest(handler, "POST", "https://shell.example.test/api/sessions", origin, "", true)
		if w.Code != 403 {
			t.Errorf("Origin %s accepted: %d", origin, w.Code)
		}
	}
	if w := libraryRequest(handler, "POST", "https://shell.example.test/api/sessions", "https://shell.example.test", "", true); w.Code != 201 {
		t.Fatalf("same-origin callback request rejected: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("POST", "https://shell.example.test/api/sessions", strings.NewReader("{}"))
	r = r.WithContext(context.WithValue(r.Context(), identityKey{}, "alice"))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-site request accepted")
	}
}

func TestLibraryFrontendModes(t *testing.T) {
	for _, external := range []bool{false, true} {
		options := libraryOptions(t)
		if external {
			options = externalOptions(t)
		}
		handler, close, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(close)
		for _, path := range []string{"/", "/./", "//", "/nested/../"} {
			page := libraryRequest(handler, "GET", "https://localhost"+path, "", "", external)
			if page.Code != 200 {
				t.Fatalf("page %s status: %d", path, page.Code)
			}
			html := page.Body.String()
			if strings.Contains(html, "{{") {
				t.Fatalf("raw template leaked at %s", path)
			}
			if !strings.Contains(html, `<script data-cfasync="false" src="/app.js" defer></script>`) {
				t.Fatal("application script allows Cloudflare Rocket Loader rewriting")
			}
			if strings.Contains(html, `id="login-form"`) == external || strings.Contains(html, `id="auth-token"`) == external {
				t.Fatalf("token form does not match authentication mode at %s", path)
			}
			if external && (!strings.Contains(html, `data-external-auth="true"`) || !strings.Contains(html, `id="external-login" href="/"`) || !strings.Contains(html, `href="/_auth/logout"`)) {
				t.Fatal("external navigation missing")
			}
		}
		head := libraryRequest(handler, "HEAD", "https://localhost/", "", "", external)
		if head.Code != 200 || head.Body.Len() != 0 {
			t.Fatal("HEAD returned a page body")
		}
	}
}

func TestLibraryValidationAndLifecycle(t *testing.T) {
	options := libraryOptions(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Options){
		"small output limit":   func(o *Options) { o.OutputLimit = 255 },
		"large output limit":   func(o *Options) { o.OutputLimit = 16<<20 + 1 },
		"few sessions":         func(o *Options) { o.MaxSessions = 0 },
		"many sessions":        func(o *Options) { o.MaxSessions = 129 },
		"missing shell":        func(o *Options) { o.Shell = "/missing-websh-shell" },
		"missing directory":    func(o *Options) { o.Dir = filepath.Join(o.Dir, "missing") },
		"file directory":       func(o *Options) { o.Dir = file },
		"missing token":        func(o *Options) { o.Token = "" },
		"short token":          func(o *Options) { o.Token = "short" },
		"long token":           func(o *Options) { o.Token = strings.Repeat("x", 257) },
		"space in token":       func(o *Options) { o.Token += " " },
		"comma in token":       func(o *Options) { o.Token += "," },
		"unsafe login URL":     func(o *Options) { o.LoginURL = "javascript:alert(1)" },
		"unsafe logout URL":    func(o *Options) { o.LogoutURL = "//evil.test" },
		"userinfo in auth URL": func(o *Options) { o.LoginURL = "https://user@auth.test/" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := options
			change(&invalid)
			handler, close, err := New(invalid)
			if err == nil || handler != nil || close != nil {
				t.Fatal("invalid options created a handler")
			}
		})
	}
	options.Token = ""
	options.Authenticate = func(*http.Request) bool { return true }
	options.OutputLimit, options.MaxSessions = 256, 1
	handler, close, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(close)
	a := handler.(*apiServer)
	if a.m.shell == "" || !filepath.IsAbs(a.m.shell) || a.m.dir != options.Dir || a.m.limit != 256 || a.m.maxSessions != 1 || len(a.m.sessions) != 0 || a.loginURL != "/" {
		t.Fatal("constructor did not apply options or eagerly started a session")
	}
	created := libraryRequest(handler, "POST", "http://localhost/api/sessions", "", "", false)
	var view sessionView
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &view) != nil {
		t.Fatalf("create session: %d %s", created.Code, created.Body.String())
	}
	close()
	close()
	closed := libraryRequest(handler, "GET", "http://localhost/api/sessions/"+view.ID, "", "", false)
	if closed.Code != 200 || json.Unmarshal(closed.Body.Bytes(), &view) != nil || view.State != "closed" {
		t.Fatal("close did not stop the shared manager's shell")
	}
	if w := libraryRequest(handler, "POST", "http://localhost/api/sessions", "", "", false); w.Code != 400 {
		t.Fatal("closed manager created a session")
	}
}
