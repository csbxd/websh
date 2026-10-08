// Package websh serves persistent browser shell sessions through an HTTP handler.
package websh

import "net/http"

const Version = "0.1.0"

// Options configures a shell manager shared by all authenticated requests.
type Options struct {
	Shell       string
	Dir         string
	OutputLimit int
	MaxSessions int
	Hostname    string
	// Token authenticates standalone clients. It is ignored when Authenticate is set.
	Token string
	// Authenticate delegates every request to the host application's verified
	// identity. Native token login, Bearer headers and cookies are then disabled.
	Authenticate func(*http.Request) bool
	// LoginURL and LogoutURL are navigation links for external authentication.
	// LoginURL defaults to "/" when Authenticate is set.
	LoginURL  string
	LogoutURL string
}
