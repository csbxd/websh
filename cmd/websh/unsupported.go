//go:build !linux && !darwin

package main

import "log"

func main() { log.Fatal("WebSH currently supports Linux and macOS. On Windows use WSL2.") }
