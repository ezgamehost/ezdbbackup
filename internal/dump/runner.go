package dump

import (
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
	stderr := &cappedBuffer{limit: r.stderrLimit()}
	cmd := exec.CommandContext(ctx, req.Binary, args...)
	cmd.Stdout = dst
	cmd.Stderr = stderr
	cmd.Env = childEnv(req.Password)
	if err := cmd.Run(); err != nil {
		cause := err
		if ctx.Err() != nil {
			cause = errors.Join(ctx.Err(), err)
		}
		return fmt.Errorf("mysqldump failed: %w: %s", cause, redact(stderr.String(), req.Password))
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

func redact(value, secret string) string {
	if secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[REDACTED]")
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
