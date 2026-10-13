//go:build windows

package main

import (
	"log"
	"os"
	"os/exec"
)

func restart() {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("restart: %v", err)
		return
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("restart: %v", err)
		return
	}
	os.Exit(0)
}
