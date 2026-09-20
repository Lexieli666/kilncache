//go:build integration

package integration

import (
	"bytes"
	"os"
	"os/exec"
)

// runCmdIn runs a command and returns its combined output. Used by test helpers
// that inspect the environment (filesystem type, docker availability) and by
// the Docker cluster tier.
func runCmdIn(dir, name string, args ...string) (string, error) {
	return runCmdFull(dir, nil, name, args...)
}

// runCmdEnv runs a command with extra environment variables appended.
func runCmdEnv(env []string, name string, args ...string) (string, error) {
	return runCmdFull("", env, name, args...)
}

func runCmdFull(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}
