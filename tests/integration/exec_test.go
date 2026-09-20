//go:build integration

package integration

import (
	"bytes"
	"os/exec"
)

// runCmdIn runs a command and returns its combined output. Used only by test
// helpers that inspect the environment (filesystem type, docker availability).
func runCmdIn(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}
