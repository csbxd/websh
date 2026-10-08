//go:build linux || darwin

package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"
)

//go:embed web/*
var assets embed.FS

const version = "0.1.0"
const cookieName = "websh_session"

type apiServer struct {
	m        *manager
	token    string
	static   http.Handler
	hostname string
}

func newHandler(m *manager, token string) http.Handler {
	web, _ := fs.Sub(assets, "web")
	return &apiServer{m: m, token: token, static: http.FileServer(http.FS(web))}
}

func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, err error) {
	jsonReply(w, status, map[string]string{"error": err.Error()})
}

func (a *apiServer) authenticated(r *http.Request) bool {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if value == r.Header.Get("Authorization") {
		value = ""
	}
	if value == "" {
		if cookie, err := r.Cookie(cookieName); err == nil {
			value = cookie.Value
		}
	}
	return a.token != "" && subtle.ConstantTimeCompare([]byte(value), []byte(a.token)) == 1
}

func (a *apiServer) allowedHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return net.ParseIP(host) != nil || strings.EqualFold(host, "localhost") || (a.hostname != "" && strings.EqualFold(host, a.hostname))
}

func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	} // CLI clients do not send browser Origin.
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return u.Scheme == scheme && strings.EqualFold(u.Host, r.Host)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	typeName, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || typeName != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return errors.New("invalid JSON request: " + err.Error())
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func (a *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if !a.allowedHost(r.Host) {
		apiError(w, http.StatusForbidden, errors.New("unrecognized Host; use localhost, an IP address, or the configured hostname"))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
		apiError(w, http.StatusForbidden, errors.New("cross-origin requests are forbidden"))
		return
	}
	if r.URL.Path == "/api/info" && r.Method == http.MethodGet {
		jsonReply(w, 200, map[string]any{"authenticated": a.authenticated(r), "shell": a.m.shell, "platform": runtime.GOOS, "output_limit": a.m.limit, "version": version})
		return
	}
	if r.URL.Path == "/api/login" && r.Method == http.MethodPost {
		var input struct {
			Token string `json:"token"`
		}
		if err := decodeJSON(w, r, &input); err != nil {
			apiError(w, 400, err)
			return
		}
		if a.token == "" || subtle.ConstantTimeCompare([]byte(input.Token), []byte(a.token)) != 1 {
			apiError(w, 401, errors.New("invalid token"))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: a.token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
		jsonReply(w, 200, map[string]bool{"authenticated": true})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if !a.authenticated(r) {
			apiError(w, 401, errors.New("authentication required"))
			return
		}
		a.api(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		apiError(w, 405, errors.New("method not allowed"))
		return
	}
	a.static.ServeHTTP(w, r)
}

func (a *apiServer) api(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	if path == "api/sessions" {
		switch r.Method {
		case http.MethodGet:
			views := a.m.list()
			sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
			jsonReply(w, 200, map[string]any{"sessions": views})
		case http.MethodPost:
			var input struct {
				Name string `json:"name"`
				Cwd  string `json:"cwd"`
			}
			if err := decodeJSON(w, r, &input); err != nil {
				apiError(w, 400, err)
				return
			}
			s, err := a.m.create(input.Name, input.Cwd)
			if err != nil {
				apiError(w, 400, err)
				return
			}
			jsonReply(w, 201, s.snapshot())
		default:
			apiError(w, 405, errors.New("method not allowed"))
		}
		return
	}
	if len(parts) < 3 || len(parts) > 4 || parts[0] != "api" || parts[1] != "sessions" {
		apiError(w, 404, errors.New("API route not found"))
		return
	}
	s := a.m.get(parts[2])
	if s == nil {
		apiError(w, 404, errors.New("session not found"))
		return
	}
	if len(parts) == 3 {
		switch r.Method {
		case http.MethodGet:
			jsonReply(w, 200, s.snapshot())
		case http.MethodDelete:
			s.close()
			jsonReply(w, 200, s.snapshot())
		default:
			apiError(w, 405, errors.New("method not allowed"))
		}
		return
	}
	if r.Method != http.MethodPost {
		apiError(w, 405, errors.New("method not allowed"))
		return
	}
	switch parts[3] {
	case "commands":
		var input struct {
			Command        string  `json:"command"`
			TimeoutSeconds float64 `json:"timeout_seconds"`
		}
		if err := decodeJSON(w, r, &input); err != nil {
			apiError(w, 400, err)
			return
		}
		if input.TimeoutSeconds < 0 || input.TimeoutSeconds > 86400 {
			apiError(w, 400, errors.New("timeout_seconds must be between 0 and 86400"))
			return
		}
		result, err := s.execute(input.Command, time.Duration(input.TimeoutSeconds*float64(time.Second)))
		if err != nil {
			status := 400
			if errors.Is(err, errBusy) {
				status = 409
			}
			if errors.Is(err, errClosed) {
				status = 410
			}
			apiError(w, status, err)
			return
		}
		jsonReply(w, 202, result)
	case "input":
		var input struct {
			Data string `json:"data"`
			EOF  bool   `json:"eof"`
		}
		if err := decodeJSON(w, r, &input); err != nil {
			apiError(w, 400, err)
			return
		}
		if err := s.sendInput(input.Data, input.EOF); err != nil {
			apiError(w, 409, err)
			return
		}
		jsonReply(w, 200, map[string]bool{"accepted": true})
	case "interrupt":
		var input struct{}
		if err := decodeJSON(w, r, &input); err != nil {
			apiError(w, 400, err)
			return
		}
		if err := s.interrupt(); err != nil {
			apiError(w, 409, err)
			return
		}
		jsonReply(w, 200, map[string]bool{"accepted": true})
	default:
		apiError(w, 404, errors.New("API route not found"))
	}
}
