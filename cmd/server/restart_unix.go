//go:build !windows

package main

import (
	"log"
	"os"
	"syscall"
)

// restart replaces this process with the (just updated) executable.
func restart() {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("restart: %v", err)
		return
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		log.Printf("restart: %v", err)
	}
}
