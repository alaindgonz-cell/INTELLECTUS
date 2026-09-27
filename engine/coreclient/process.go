package coreclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// Process is a spawned `intellectus-core serve` child. The engine process
// that spawned it is the authenticated engine principal: it alone holds
// the pipe.
type Process struct {
	cmd    *exec.Cmd
	Client *Client
}

// Spawn starts `bin serveArgs...` (e.g. "serve", "--db", path) and returns
// a client wired to its stdin/stdout. stderr (core logs) goes to logs.
func Spawn(bin string, serveArgs []string, logs io.Writer) (*Process, error) {
	cmd := exec.Command(bin, serveArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if logs == nil {
		logs = io.Discard
	}
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", bin, err)
	}
	return &Process{cmd: cmd, Client: NewClient(stdout, stdin)}, nil
}

// Close closes the core's stdin, waits up to timeout for it to exit on its
// own, then kills it. Returns the process exit error, if any.
func (p *Process) Close(timeout time.Duration) error {
	_ = p.Client.Close()
	select {
	case <-p.Client.Done():
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		<-p.Client.Done()
	}
	return p.cmd.Wait()
}

// Kill terminates the core immediately (used to simulate a crash).
func (p *Process) Kill() error {
	err := p.cmd.Process.Kill()
	<-p.Client.Done()
	_ = p.cmd.Wait()
	return err
}

// RunCLI runs a one-shot core subcommand (init, replay, keygen, ...) and
// returns its stdout; a non-zero exit is an error carrying stderr.
func RunCLI(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), fmt.Errorf("%s %v: exit %d: %s", bin, args, ee.ExitCode(), bytes.TrimSpace(errb.Bytes()))
		}
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}
