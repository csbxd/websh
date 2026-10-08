//go:build linux || darwin

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/csbxd/websh"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	shell := flag.String("shell", "/bin/sh", "POSIX-compatible shell executable")
	dir := flag.String("dir", ".", "initial shell working directory")
	limit := flag.Int("output-limit", 1<<20, "maximum retained bytes per stdout/stderr stream; exceeding stops command")
	max := flag.Int("max-sessions", 16, "maximum simultaneously open shell sessions")
	hostname := flag.String("hostname", "", "additional allowed HTTP hostname")
	flag.Parse()
	token := os.Getenv("WEBSH_TOKEN")
	if token == "" {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			log.Fatal(err)
		}
		token = hex.EncodeToString(secret[:])
	}
	handler, closeWebSH, err := websh.New(websh.Options{
		Shell: *shell, Dir: *dir, OutputLimit: *limit, MaxSessions: *max,
		Hostname: *hostname, Token: token,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer closeWebSH()
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
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		closeWebSH()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Printf("WebSH %s\nURL: http://%s\nToken: %s\nShell: %s\nDirectory: %s\n", websh.Version, listener.Addr(), token, resolved, workingDir)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
