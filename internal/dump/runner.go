package dump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

const defaultStderrLimit int64 = 64 << 10

// Runner executes validated mysqldump requests.
type Runner interface {
	Run(context.Context, Request, io.Writer) error
	Probe(context.Context, Request) error
}

// FailureKind identifies whether mysqldump failed to start or failed after it
// became a running process.
type FailureKind string

const (
	FailureStartup   FailureKind = "startup"
	FailureExecution FailureKind = "execution"
)

// RunError classifies a mysqldump process failure and preserves its cause.
type RunError struct {
	Kind FailureKind
	Err  error
}

func (e *RunError) Error() string { return fmt.Sprintf("mysqldump failed: %v", e.Err) }

func (e *RunError) Unwrap() error { return e.Err }

// ExecRunner invokes mysqldump as a child process. StderrLimit controls the
// maximum stderr included in a command failure; zero selects 64 KiB.
type ExecRunner struct {
	StderrLimit int64
}

// Run writes the mysqldump output to dst.
func (r ExecRunner) Run(ctx context.Context, req Request, dst io.Writer) error {
	return r.run(ctx, req, Args(req), dst)
}

// Probe verifies mysqldump connectivity and permissions without producing an
// output file or writing any dump data.
func (r ExecRunner) Probe(ctx context.Context, req Request) error {
	return r.run(ctx, req, ProbeArgs(req), io.Discard)
}

func (r ExecRunner) run(ctx context.Context, req Request, args []string, dst io.Writer) error {
	stderr := newRedactingCappedBuffer(r.stderrLimit(), req.Password)
	cmd := exec.CommandContext(ctx, req.Binary, args...)
	cmd.Stdout = dst
	cmd.Stderr = stderr
	cmd.Env = childEnv(req.Password)
	err := cmd.Run()
	stderr.finish()
	if err != nil {
		kind := FailureExecution
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			kind = FailureStartup
		}
		cause := err
		if ctx.Err() != nil {
			cause = errors.Join(ctx.Err(), err)
		}
		return &RunError{Kind: kind, Err: fmt.Errorf("%w: %s", cause, stderr.String())}
	}
	return nil
}

func (r ExecRunner) stderrLimit() int64 {
	if r.StderrLimit <= 0 {
		return defaultStderrLimit
	}
	return r.StderrLimit
}

func childEnv(password string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "MYSQL_PWD=") {
			env = append(env, entry)
		}
	}
	if password != "" {
		env = append(env, "MYSQL_PWD="+password)
	}
	return env
}

type cappedBuffer struct {
	limit int64
	data  []byte
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	remaining := b.limit - int64(len(b.data))
	if remaining > 0 {
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	return written, nil
}

func (b *cappedBuffer) String() string {
	return string(b.data)
}

type redactingCappedBuffer struct {
	output  cappedBuffer
	secret  []byte
	pending []byte
}

func newRedactingCappedBuffer(limit int64, secret string) *redactingCappedBuffer {
	return &redactingCappedBuffer{output: cappedBuffer{limit: limit}, secret: []byte(secret)}
}

func (b *redactingCappedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if len(b.secret) == 0 {
		_, _ = b.output.Write(p)
		return written, nil
	}
	for _, value := range p {
		b.pending = append(b.pending, value)
		b.flushCompleteSecrets()
	}
	return written, nil
}

func (b *redactingCappedBuffer) flushCompleteSecrets() {
	for len(b.pending) >= len(b.secret) {
		if bytes.HasPrefix(b.pending, b.secret) {
			_, _ = b.output.Write([]byte("[REDACTED]"))
			b.pending = b.pending[len(b.secret):]
			continue
		}
		_, _ = b.output.Write(b.pending[:1])
		b.pending = b.pending[1:]
	}
}

func (b *redactingCappedBuffer) finish() {
	if len(b.pending) == 0 {
		return
	}
	prefixLength := trailingSecretPrefixLength(b.pending, b.secret)
	if prefixLength > 0 {
		_, _ = b.output.Write(b.pending[:len(b.pending)-prefixLength])
		_, _ = b.output.Write([]byte("[REDACTED]"))
	} else {
		_, _ = b.output.Write(b.pending)
	}
	b.pending = nil
}

func (b *redactingCappedBuffer) String() string {
	return b.output.String()
}

func trailingSecretPrefixLength(value, secret []byte) int {
	for length := min(len(value), len(secret)-1); length > 0; length-- {
		if bytes.Equal(value[len(value)-length:], secret[:length]) {
			return length
		}
	}
	return 0
}
