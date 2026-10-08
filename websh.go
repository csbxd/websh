//go:build linux || darwin

package websh

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// New validates options and returns an HTTP handler without opening a listener
// or starting a shell. Close stops every session and may be called repeatedly.
func New(opts Options) (handler http.Handler, close func(), err error) {
	if opts.OutputLimit < 256 || opts.OutputLimit > 16<<20 {
		return nil, nil, errors.New("output-limit must be between 256 and 16777216")
	}
	if opts.MaxSessions < 1 || opts.MaxSessions > 128 {
		return nil, nil, errors.New("max-sessions must be between 1 and 128")
	}
	shell, err := exec.LookPath(opts.Shell)
	if err != nil {
		return nil, nil, fmt.Errorf("shell: %w", err)
	}
	shell, err = filepath.Abs(shell)
	if err != nil {
		return nil, nil, err
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil, errors.New("dir must be an existing directory")
	}
	if opts.Authenticate == nil {
		if len(opts.Token) < 16 || len(opts.Token) > 256 || strings.IndexFunc(opts.Token, func(r rune) bool {
			return r < 33 || r > 126 || strings.ContainsRune("\";\\,", r)
		}) >= 0 {
			return nil, nil, errors.New("token must have 16 to 256 printable ASCII characters without spaces, quotes, semicolons, commas or backslashes")
		}
	} else {
		opts.Token = ""
		if opts.LoginURL == "" {
			opts.LoginURL = "/"
		}
	}
	for _, target := range []string{opts.LoginURL, opts.LogoutURL} {
		if target != "" && !validAuthURL(target) {
			return nil, nil, errors.New("login and logout URLs must be absolute HTTP(S) URLs or paths starting with /")
		}
	}
	m := newManager(shell, dir, opts.OutputLimit)
	m.maxSessions = opts.MaxSessions
	a := newHandler(m, opts.Token).(*apiServer)
	a.hostname, a.authenticate = opts.Hostname, opts.Authenticate
	a.loginURL, a.logoutURL = opts.LoginURL, opts.LogoutURL
	return a, m.close, nil
}

func validAuthURL(target string) bool {
	u, err := url.Parse(target)
	if err != nil || u.User != nil || strings.ContainsAny(target, "\r\n\\") {
		return false
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return u.Host != ""
	}
	return u.Scheme == "" && u.Host == "" && strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(target, "//")
}
