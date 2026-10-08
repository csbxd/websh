//go:build linux || darwin

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	shell := flag.String("shell", "/bin/sh", "POSIX-compatible shell executable")
	dir := flag.String("dir", ".", "initial shell working directory")
	limit := flag.Int("output-limit", 1<<20, "maximum retained bytes per stdout/stderr stream; exceeding stops command")
	max := flag.Int("max-sessions", 16, "maximum simultaneously open shell sessions")
	hostname := flag.String("hostname", "", "additional allowed HTTP hostname")
	flag.Parse()
	if *limit < 256 || *limit > 16<<20 {
		log.Fatal("output-limit must be between 256 and 16777216")
	}
	if *max < 1 || *max > 128 {
		log.Fatal("max-sessions must be between 1 and 128")
	}
	resolved, err := exec.LookPath(*shell)
	if err != nil {
		log.Fatal(err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		log.Fatal(err)
	}
	workingDir, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatal(err)
	}
	info, err := os.Stat(workingDir)
	if err != nil || !info.IsDir() {
		log.Fatal("-dir must be an existing directory")
	}
	token := os.Getenv("WEBSH_TOKEN")
	if token == "" {
		token = randomID() + randomID()
	}
	if len(token) < 16 || len(token) > 256 || strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 || strings.ContainsRune("\";\\,", r) }) >= 0 {
		log.Fatal("WEBSH_TOKEN must have 16 to 256 printable ASCII characters without spaces, quotes, semicolons, commas or backslashes")
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	m := newManager(resolved, workingDir, *limit)
	m.maxSessions = *max
	defer m.close()
	handler := newHandler(m, token).(*apiServer)
	handler.hostname = *hostname
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		m.close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Printf("WebSH %s\nURL: http://%s\nToken: %s\nShell: %s\nDirectory: %s\n", version, listener.Addr(), token, resolved, workingDir)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
