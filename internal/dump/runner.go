package dump

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const defaultStderrLimit int64 = 64 << 10
const defaultWaitDelay = 250 * time.Millisecond

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
	stdout := &errorRecordingWriter{dst: dst}
	cmd := exec.CommandContext(ctx, req.Binary, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = childEnv(req.Password)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process) }
	cmd.WaitDelay = defaultWaitDelay
	if err := cmd.Start(); err != nil {
		stderr.finish()
		return &RunError{Kind: FailureStartup, Err: fmt.Errorf("%w: %s", err, stderr.String())}
	}
	err := cmd.Wait()
	if copyErr := stdout.Err(); copyErr != nil && !errors.Is(err, copyErr) {
		err = errors.Join(err, copyErr)
	}
	if err != nil {
		_ = killProcessGroup(cmd.Process)
	}
	stderr.finish()
	if err != nil {
		cause := err
		if ctx.Err() != nil {
			cause = errors.Join(ctx.Err(), err)
		}
		return &RunError{Kind: FailureExecution, Err: fmt.Errorf("%w: %s", cause, stderr.String())}
	}
	return nil
}

type errorRecordingWriter struct {
	dst io.Writer
	mu  sync.Mutex
	err error
}

func (w *errorRecordingWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if err != nil {
		w.mu.Lock()
		if w.err == nil {
			w.err = err
		}
		w.mu.Unlock()
	}
	return n, err
}

func (w *errorRecordingWriter) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func killProcessGroup(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
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
		name, _, _ := strings.Cut(entry, "=")
		if name == "MYSQL_PWD" {
			continue
		}
		if _, sensitive := cloudCredentialEnvironment[name]; sensitive {
			continue
		}
		env = append(env, entry)
	}
	if password != "" {
		env = append(env, "MYSQL_PWD="+password)
	}
	return env
}

var cloudCredentialEnvironment = map[string]struct{}{
	"AMAZON_ACCESS_KEY_ID":                   {},
	"AMAZON_ACCESS_KEY":                      {},
	"AMAZON_SECURITY_TOKEN":                  {},
	"AMAZON_SECRET_ACCESS_KEY":               {},
	"AMAZON_SECRET_KEY":                      {},
	"AWS_ACCESS_KEY":                         {},
	"AWS_ACCESS_KEY_ID":                      {},
	"AWS_CONFIG_FILE":                        {},
	"AWS_CONTAINER_AUTHORIZATION_TOKEN":      {},
	"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": {},
	"AWS_CONTAINER_CREDENTIALS_FULL_URI":     {},
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": {},
	"AWS_CREDENTIAL_FILE":                    {},
	"AWS_CREDENTIAL_PROFILES_FILE":           {},
	"AWS_CREDENTIALS_FILE":                   {},
	"AWS_DEFAULT_PROFILE":                    {},
	"AWS_PROFILE":                            {},
	"AWS_ROLE_ARN":                           {},
	"AWS_ROLE_SESSION_NAME":                  {},
	"AWS_SECRET_ACCESS_KEY":                  {},
	"AWS_SECRET_KEY":                         {},
	"AWS_SECURITY_TOKEN":                     {},
	"AWS_SESSION_TOKEN":                      {},
	"AWS_SHARED_CREDENTIALS_FILE":            {},
	"AWS_WEB_IDENTITY_TOKEN_FILE":            {},
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
	failure []int
	matched int
}

func newRedactingCappedBuffer(limit int64, secret string) *redactingCappedBuffer {
	secretBytes := []byte(secret)
	return &redactingCappedBuffer{
		output:  cappedBuffer{limit: limit},
		secret:  secretBytes,
		failure: prefixFailureTable(secretBytes),
	}
}

func (b *redactingCappedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if len(b.secret) == 0 {
		_, _ = b.output.Write(p)
		return written, nil
	}
	for _, value := range p {
		if int64(len(b.output.data)) >= b.output.limit {
			b.matched = 0
			break
		}
		previous := b.matched
		for b.matched > 0 && b.secret[b.matched] != value {
			b.matched = b.failure[b.matched-1]
		}
		if b.secret[b.matched] == value {
			if b.matched < previous {
				_, _ = b.output.Write(b.secret[:previous-b.matched])
			}
			b.matched++
			if b.matched == len(b.secret) {
				_, _ = b.output.Write([]byte("[REDACTED]"))
				b.matched = 0
			}
			continue
		}
		if previous > 0 {
			_, _ = b.output.Write(b.secret[:previous])
		}
		_, _ = b.output.Write([]byte{value})
	}
	return written, nil
}

func prefixFailureTable(pattern []byte) []int {
	failure := make([]int, len(pattern))
	for i, matched := 1, 0; i < len(pattern); i++ {
		for matched > 0 && pattern[matched] != pattern[i] {
			matched = failure[matched-1]
		}
		if pattern[matched] == pattern[i] {
			matched++
		}
		failure[i] = matched
	}
	return failure
}

func (b *redactingCappedBuffer) finish() {
	if b.matched == 0 {
		return
	}
	_, _ = b.output.Write([]byte("[REDACTED]"))
	b.matched = 0
}

func (b *redactingCappedBuffer) String() string {
	return b.output.String()
}
