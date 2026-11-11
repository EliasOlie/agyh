package bridge

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Runner starts an agent headlessly for /new.
type Runner interface {
	Run(ctx context.Context, dir string, argv []string) (string, error)
}

// ExecRunner runs the agent as a child process (no shell: the prompt is a
// single argv element, so chat text can never be interpreted by a shell).
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("empty runner command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}
