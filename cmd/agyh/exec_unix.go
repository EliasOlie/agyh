//go:build unix

package main

import (
	"os"
	"syscall"
)

// execReplace turns this process into the agent so it owns the terminal.
func execReplace(bin string, argv []string) error {
	return syscall.Exec(bin, argv, os.Environ())
}
