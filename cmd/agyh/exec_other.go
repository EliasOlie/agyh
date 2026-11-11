//go:build !unix

package main

import (
	"os"
	"os/exec"
)

// execReplace runs the agent as a child process where exec(2) is unavailable.
func execReplace(bin string, argv []string) error {
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}
