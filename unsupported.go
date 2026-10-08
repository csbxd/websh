//go:build !linux && !darwin

package websh

import (
	"errors"
	"net/http"
)

func New(opts Options) (http.Handler, func(), error) {
	return nil, nil, errors.New("WebSH currently supports Linux and macOS. On Windows use WSL2")
}
