package dump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This fails if dump output is not streamed to the supplied destination, or
// if the child receives inherited credentials rather than the resolved one.
func TestExecRunnerRunStreamsOutputAndIsolatesPassword(t *testing.T) {
	t.Setenv("MYSQL_PWD", "inherited-password")
	passwordPath := filepath.Join(t.TempDir(), "password")
	argsPath := filepath.Join(t.TempDir(), "args")
	binary := writeFixture(t, "success", passwordPath, argsPath)

	var output bytes.Buffer
	err := (ExecRunner{}).Run(context.Background(), Request{
		Binary: binary, Host: "db.internal", Port: 3307, User: "backup",
		Password: "resolved-password", Databases: []string{"app"},
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "dump contents\n"; got != want {
		t.Fatalf("dump output = %q, want %q", got, want)
	}
	if got := readFixtureFile(t, passwordPath); got != "resolved-password\n" {
		t.Fatalf("child MYSQL_PWD = %q, want resolved password", got)
	}
	if got := os.Getenv("MYSQL_PWD"); got != "inherited-password" {
		t.Fatalf("parent MYSQL_PWD = %q, want inherited value", got)
	}
	if got, want := strings.Fields(readFixtureFile(t, argsPath)), []string{
		"--no-defaults", "--host=db.internal", "--port=3307", "--user=backup", "--databases", "--", "app",
	}; !slices.Equal(got, want) {
		t.Fatalf("arguments = %#v, want %#v", got, want)
	}
}

func TestExecRunnerPreservesShebangScriptArgvZero(t *testing.T) {
	directory := secureDumpTestDir(t)
	argvZeroPath := filepath.Join(directory, "argv-zero")
	script := filepath.Join(directory, "mysqldump-wrapper")
	contents := "#!/bin/sh\nprintf '%s\\n' \"$0\" > " + shellQuote(argvZeroPath) + "\nprintf 'dump contents\\n'\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := (ExecRunner{}).Run(context.Background(), Request{Binary: script}, &output); err != nil {
		t.Fatalf("Run(script) error = %v", err)
	}
	if got := strings.TrimSpace(readFixtureFile(t, argvZeroPath)); got != script {
		t.Fatalf("script argv[0] = %q, want configured path %q", got, script)
	}
	if output.String() != "dump contents\n" {
		t.Fatalf("script output = %q", output.String())
	}
}

// Following req.Binary after this hook without rechecking its trusted path
// association would run the attacker fixture. The invocation must fail closed
// and close its validation descriptor.
func TestExecRunnerRejectsFinalSymlinkSwapAfterDescriptorValidation(t *testing.T) {
	directory := secureDumpTestDir(t)
	trusted := filepath.Join(directory, "trusted")
	attacker := filepath.Join(directory, "attacker")
	attackerMarker := filepath.Join(directory, "attacker-ran")
	if err := os.WriteFile(trusted, []byte("#!/bin/sh\nprintf 'trusted output\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attacker, []byte("#!/bin/sh\n: > "+shellQuote(attackerMarker)+"\nprintf 'attacker output\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "mysqldump")
	if err := os.Symlink(trusted, link); err != nil {
		t.Fatal(err)
	}
	var pinned *os.File
	runner := ExecRunner{afterExecutableOpen: func(file *os.File) {
		pinned = file
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(attacker, link); err != nil {
			t.Fatal(err)
		}
	}}
	var output bytes.Buffer
	err := runner.Run(context.Background(), Request{Binary: link}, &output)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("Run(swapped path) error = %v, want path-association rejection", err)
	}
	if got := output.String(); got != "" {
		t.Fatalf("dump output = %q, want no executable output", got)
	}
	if _, err := os.Stat(attackerMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attacker marker stat error = %v, want attacker never executed", err)
	}
	if pinned == nil {
		t.Fatal("executable-open hook did not receive descriptor")
	}
	if _, err := pinned.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("pinned executable Stat() error = %v, want closed descriptor", err)
	}
}

// This fails if a missing resolved password accidentally leaks MYSQL_PWD from
// the parent into mysqldump.
func TestExecRunnerRunRemovesInheritedPasswordWhenRequestHasNone(t *testing.T) {
	t.Setenv("MYSQL_PWD", "inherited-password")
	passwordPath := filepath.Join(t.TempDir(), "password")
	binary := writeFixture(t, "success", passwordPath, filepath.Join(t.TempDir(), "args"))

	if err := (ExecRunner{}).Run(context.Background(), Request{
		Binary: binary, Host: "db.internal", Port: 3307, User: "backup", Databases: []string{"app"},
	}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := readFixtureFile(t, passwordPath); got != "\n" {
		t.Fatalf("child MYSQL_PWD = %q, want empty", got)
	}
}

// This fails if Probe performs a normal dump rather than the no-data probe.
func TestExecRunnerProbeUsesProbeArguments(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "args")
	binary := writeFixture(t, "success", filepath.Join(t.TempDir(), "password"), argsPath)
	req := Request{Binary: binary, Host: "db.internal", Port: 3307, User: "backup", Databases: []string{"app"}}

	if err := (ExecRunner{}).Probe(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--no-defaults", "--host=db.internal", "--port=3307", "--user=backup",
		"--no-data", "--no-create-info", "--skip-triggers",
		"--skip-lock-tables", "--skip-delete-master-logs", "--skip-flush-logs",
		"--databases", "--", "app",
	}
	if got := strings.Fields(readFixtureFile(t, argsPath)); !slices.Equal(got, want) {
		t.Fatalf("probe arguments = %#v, want %#v", got, want)
	}
}

// This fails if command failures omit useful bounded stderr or disclose the
// resolved password while reporting them.
func TestExecRunnerRunReportsStderrWithoutPassword(t *testing.T) {
	password := "resolved-password"
	binary := writeFixture(t, "failure", filepath.Join(t.TempDir(), "password"), filepath.Join(t.TempDir(), "args"))
	err := (ExecRunner{}).Run(context.Background(), Request{
		Binary: binary, Host: "db.internal", Port: 3307, User: "backup", Password: password, Databases: []string{"app"},
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("Run() error = nil, want command failure")
	}
	if got := err.Error(); !strings.Contains(got, "exit status 23") || !strings.Contains(got, "fixture failure") {
		t.Fatalf("Run() error = %q, want exit status and stderr", got)
	} else if strings.Contains(got, password) {
		t.Fatalf("Run() error disclosed password: %q", got)
	}
}

func TestExecRunnerClassifiesProcessStartupFailure(t *testing.T) {
	err := (ExecRunner{}).Run(context.Background(), Request{
		Binary: filepath.Join(t.TempDir(), "missing-mysqldump"),
	}, &bytes.Buffer{})
	var runErr *RunError
	if !errors.As(err, &runErr) {
		t.Fatalf("Run() error type = %T, want *RunError", err)
	}
	if runErr.Kind != FailureStartup {
		t.Fatalf("Run() failure kind = %q, want %q", runErr.Kind, FailureStartup)
	}
}

func TestExecRunnerClassifiesProcessExecutionFailure(t *testing.T) {
	binary := writeFixture(t, "failure", filepath.Join(t.TempDir(), "password"), filepath.Join(t.TempDir(), "args"))
	err := (ExecRunner{}).Run(context.Background(), Request{Binary: binary}, &bytes.Buffer{})
	var runErr *RunError
	if !errors.As(err, &runErr) {
		t.Fatalf("Run() error type = %T, want *RunError", err)
	}
	if runErr.Kind != FailureExecution {
		t.Fatalf("Run() failure kind = %q, want %q", runErr.Kind, FailureExecution)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Run() error = %v, want original exec.ExitError cause", err)
	}
}

// This fails if a stdout copy failure that happens after a successful Start is
// mislabeled as a process-startup failure or loses the writer's error identity.
func TestExecRunnerClassifiesPostStartCopyFailureAsExecution(t *testing.T) {
	for _, mode := range []string{"success", "largeoutput"} {
		t.Run(mode, func(t *testing.T) {
			copyErr := errors.New("destination rejected dump output")
			binary := writeFixture(t, mode, filepath.Join(t.TempDir(), "password"), filepath.Join(t.TempDir(), "args"))
			err := (ExecRunner{}).Run(context.Background(), Request{Binary: binary}, errorWriter{err: copyErr})
			var runErr *RunError
			if !errors.As(err, &runErr) {
				t.Fatalf("Run() error type = %T, want *RunError", err)
			}
			if runErr.Kind != FailureExecution {
				t.Fatalf("Run() failure kind = %q, want %q", runErr.Kind, FailureExecution)
			}
			if !errors.Is(err, copyErr) {
				t.Fatalf("Run() error = %v, want destination error identity", err)
			}
		})
	}
}

// This fails if stderr is capped before password redaction, allowing a prefix
// of the configured password to escape at the diagnostic boundary.
func TestExecRunnerRunRedactsPasswordBeforeStderrCap(t *testing.T) {
	password := "SensitiveToken9"
	binary := writeFixture(t, "passwordfailure", filepath.Join(t.TempDir(), "password"), filepath.Join(t.TempDir(), "args"))
	err := (ExecRunner{StderrLimit: 5}).Run(context.Background(), Request{
		Binary: binary, Host: "db.internal", Port: 3307, User: "backup", Password: password, Databases: []string{"app"},
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("Run() error = nil, want command failure")
	}
	for size := 1; size <= len(password); size++ {
		if leaked := password[:size]; strings.Contains(err.Error(), leaked) {
			t.Fatalf("Run() error leaked password fragment %q: %q", leaked, err)
		}
	}
}

// This fails if stderr ending in any proper password prefix is emitted after
// finalization, including when the prefix arrives through separate writes.
func TestExecRunnerRunSuppressesTrailingPasswordPrefixes(t *testing.T) {
	const password = "SensitiveToken9"
	type testCase struct {
		name   string
		writes []string
	}
	var tests []testCase
	for prefixLength := 1; prefixLength < len(password); prefixLength++ {
		prefix := password[:prefixLength]
		tests = append(tests, testCase{name: fmt.Sprintf("prefix-%d", prefixLength), writes: []string{prefix}})
		if prefixLength > 1 {
			tests = append(tests, testCase{name: fmt.Sprintf("prefix-%d-split", prefixLength), writes: []string{prefix[:1], prefix[1:]}})
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binary := writeStderrFailureFixture(t, tt.writes)
			err := (ExecRunner{StderrLimit: 64}).Run(context.Background(), Request{
				Binary: binary, Host: "db.internal", Port: 3307, User: "backup", Password: password, Databases: []string{"app"},
			}, &bytes.Buffer{})
			if err == nil {
				t.Fatal("Run() error = nil, want command failure")
			}
			for size := 1; size < len(password); size++ {
				if leaked := password[:size]; strings.Contains(err.Error(), leaked) {
					t.Fatalf("Run() error leaked password prefix %q: %q", leaked, err)
				}
			}
		})
	}
}

func TestRedactingCappedBufferMatchesGreedyReplacementAcrossWriteSplits(t *testing.T) {
	tests := []struct {
		secret string
		input  string
	}{
		{secret: "abc", input: "xabcx"},
		{secret: "aab", input: "aaab"},
		{secret: "abab", input: "ababab"},
		{secret: "aaa", input: "aaaa"},
		{secret: "abac", input: "ababac"},
		{secret: "SensitiveToken9", input: "prefix-SensitiveToken9-tail-Sens"},
	}
	for _, tt := range tests {
		for split := 1; split <= len(tt.input); split++ {
			name := fmt.Sprintf("%s/%d", tt.secret, split)
			t.Run(name, func(t *testing.T) {
				buffer := newRedactingCappedBuffer(1<<20, tt.secret)
				for offset := 0; offset < len(tt.input); offset += split {
					end := min(offset+split, len(tt.input))
					if _, err := buffer.Write([]byte(tt.input[offset:end])); err != nil {
						t.Fatal(err)
					}
				}
				buffer.finish()
				if got, want := buffer.String(), referenceRedaction(tt.input, tt.secret); got != want {
					t.Fatalf("redaction = %q, want %q", got, want)
				}
			})
		}
	}
}

// This fails if bounded stderr makes a successful dump fail with a short
// write, or if more than the configured diagnostic budget is retained.
func TestExecRunnerRunCapsStderrWithoutInterruptingSuccessfulDump(t *testing.T) {
	binary := writeFixture(t, "loudsuccess", filepath.Join(t.TempDir(), "password"), filepath.Join(t.TempDir(), "args"))
	var output bytes.Buffer
	err := (ExecRunner{StderrLimit: 16}).Run(context.Background(), Request{
		Binary: binary, Host: "db.internal", Port: 3307, User: "backup", Databases: []string{"app"},
	}, &output)
	if err != nil {
		t.Fatalf("Run() error = %v, want successful dump", err)
	}
	if got, want := output.String(), "dump contents\n"; got != want {
		t.Fatalf("dump output = %q, want %q", got, want)
	}
}

// This fails if a cancelled context does not terminate the mysqldump process.
func TestExecRunnerRunHonorsContextCancellation(t *testing.T) {
	startedPath := filepath.Join(t.TempDir(), "started")
	grandchildPIDPath := filepath.Join(t.TempDir(), "grandchild-pid")
	t.Setenv("EZDBBACKUP_STARTED_PATH", startedPath)
	t.Setenv("EZDBBACKUP_GRANDCHILD_PID_PATH", grandchildPIDPath)
	binary := writeFixture(t, "wait", filepath.Join(t.TempDir(), "password"), filepath.Join(t.TempDir(), "args"))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- (ExecRunner{}).Run(ctx, Request{
			Binary: binary, Host: "db.internal", Port: 3307, User: "backup", Databases: []string{"app"},
		}, &bytes.Buffer{})
	}()
	waitForFixtureFile(t, startedPath)
	grandchildPID := readFixturePID(t, grandchildPIDPath)
	started := time.Now()
	cancel()
	var err error
	select {
	case err = <-result:
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after cancellation")
	}
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Run() error = nil, want cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("Run() cancellation took %v; grandchild inherited process pipes", elapsed)
	}
	waitForProcessExit(t, grandchildPID)
}

// This fails if a wrapper exits while a grandchild retains stdout/stderr and
// Wait is allowed to block indefinitely on the inherited pipes.
func TestExecRunnerBoundsWaitForGrandchildHoldingPipes(t *testing.T) {
	startedPath := filepath.Join(t.TempDir(), "started")
	grandchildPIDPath := filepath.Join(t.TempDir(), "grandchild-pid")
	t.Setenv("EZDBBACKUP_STARTED_PATH", startedPath)
	t.Setenv("EZDBBACKUP_GRANDCHILD_PID_PATH", grandchildPIDPath)
	binary := writeFixture(t, "orphan", filepath.Join(t.TempDir(), "password"), filepath.Join(t.TempDir(), "args"))
	started := time.Now()
	err := (ExecRunner{}).Run(context.Background(), Request{Binary: binary}, &bytes.Buffer{})
	elapsed := time.Since(started)
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Kind != FailureExecution {
		t.Fatalf("Run() error = %#v, want bounded execution failure", err)
	}
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Run() error = %v, want exec.ErrWaitDelay", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("Run() waited %v for inherited pipes, want bounded wait", elapsed)
	}
	waitForFixtureFile(t, startedPath)
	waitForProcessExit(t, readFixturePID(t, grandchildPIDPath))
}

// This fails if the mysqldump child inherits AWS credentials/config pointers,
// or if scrubbing removes ordinary environment needed to execute normally.
func TestExecRunnerScrubsCloudCredentialsFromChildEnvironment(t *testing.T) {
	expected := []string{
		"AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY", "AMAZON_ACCESS_KEY_ID", "AMAZON_ACCESS_KEY",
		"AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY", "AMAZON_SECRET_ACCESS_KEY", "AMAZON_SECRET_KEY",
		"AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN", "AMAZON_SECURITY_TOKEN",
		"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_CONFIG_FILE",
		"AWS_SHARED_CREDENTIALS_FILE", "AWS_CREDENTIAL_FILE", "AWS_CREDENTIALS_FILE",
		"AWS_CREDENTIAL_PROFILES_FILE",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_AUTHORIZATION_TOKEN",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_ROLE_ARN", "AWS_ROLE_SESSION_NAME",
	}
	for _, name := range expected {
		if _, ok := cloudCredentialEnvironment[name]; !ok {
			t.Fatalf("cloudCredentialEnvironment is missing %s", name)
		}
	}
	for name := range cloudCredentialEnvironment {
		t.Setenv(name, "must-not-leak")
	}
	t.Setenv("EZDBBACKUP_NORMAL_ENV", "preserved")
	envPath := filepath.Join(t.TempDir(), "environment")
	binary := writeEnvironmentFixture(t, envPath)

	if err := (ExecRunner{}).Run(context.Background(), Request{
		Binary: binary, Password: "job-password", Databases: []string{"app"},
	}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	child := parseEnvironment(readFixtureFile(t, envPath))
	for name := range cloudCredentialEnvironment {
		if value, ok := child[name]; ok {
			t.Fatalf("child environment contains %s=%q", name, value)
		}
	}
	if got := child["EZDBBACKUP_NORMAL_ENV"]; got != "preserved" {
		t.Fatalf("ordinary child environment = %q, want preserved", got)
	}
	if got := child["MYSQL_PWD"]; got != "job-password" {
		t.Fatalf("child MYSQL_PWD = %q, want job password", got)
	}
}

func referenceRedaction(input, secret string) string {
	if secret == "" {
		return input
	}
	var output strings.Builder
	for {
		index := strings.Index(input, secret)
		if index >= 0 {
			output.WriteString(input[:index])
			output.WriteString("[REDACTED]")
			input = input[index+len(secret):]
			continue
		}
		prefix := 0
		for length := min(len(input), len(secret)-1); length > 0; length-- {
			if strings.HasSuffix(input, secret[:length]) {
				prefix = length
				break
			}
		}
		output.WriteString(input[:len(input)-prefix])
		if prefix > 0 {
			output.WriteString("[REDACTED]")
		}
		return output.String()
	}
}

func writeFixture(t *testing.T, mode, passwordPath, argsPath string) string {
	t.Helper()
	path := filepath.Join(secureDumpTestDir(t), "mysqldump-fixture")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"${MYSQL_PWD}\" > " + shellQuote(passwordPath) + "\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\n" +
		"case " + shellQuote(mode) + " in\n" +
		"success) printf 'dump contents\\n' ;;\n" +
		"failure) printf 'fixture failure\\n' >&2; exit 23 ;;\n" +
		"passwordfailure) printf '%s' \"${MYSQL_PWD}\" >&2; exit 23 ;;\n" +
		"loudsuccess) i=0; while [ \"$i\" -lt 100 ]; do printf x >&2; i=$((i + 1)); done; printf 'dump contents\\n' ;;\n" +
		"largeoutput) dd if=/dev/zero bs=1048576 count=2 2>/dev/null ;;\n" +
		"wait) sleep 10 & grandchild=$!; printf '%s' \"$grandchild\" > \"${EZDBBACKUP_GRANDCHILD_PID_PATH}\"; : > \"${EZDBBACKUP_STARTED_PATH}\"; wait ;;\n" +
		"orphan) sleep 10 & grandchild=$!; printf '%s' \"$grandchild\" > \"${EZDBBACKUP_GRANDCHILD_PID_PATH}\"; : > \"${EZDBBACKUP_STARTED_PATH}\"; exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeEnvironmentFixture(t *testing.T, envPath string) string {
	t.Helper()
	path := filepath.Join(secureDumpTestDir(t), "mysqldump-environment-fixture")
	script := "#!/bin/sh\n" +
		"env > " + shellQuote(envPath) + "\n" +
		"printf 'dump contents\\n'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func parseEnvironment(contents string) map[string]string {
	environment := make(map[string]string)
	for _, line := range strings.Split(contents, "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			environment[name] = value
		}
	}
	return environment
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func writeStderrFailureFixture(t *testing.T, writes []string) string {
	t.Helper()
	path := filepath.Join(secureDumpTestDir(t), "mysqldump-stderr-fixture")
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	for _, value := range writes {
		script.WriteString("printf '%s' ")
		script.WriteString(shellQuote(value))
		script.WriteString(" >&2\n")
	}
	script.WriteString("exit 23\n")
	if err := os.WriteFile(path, []byte(script.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func secureDumpTestDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\\"'\\\"'") + "'"
}

func readFixtureFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func waitForFixtureFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fixture did not create %s", path)
}

func readFixturePID(t *testing.T, path string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(readFixtureFile(t, path)))
	if err != nil || pid <= 0 {
		t.Fatalf("fixture PID in %s is invalid: %q (%v)", path, readFixtureFile(t, path), err)
	}
	return pid
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	path := fmt.Sprintf("/proc/%d/stat", pid)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if end := strings.LastIndexByte(string(data), ')'); end >= 0 {
			fields := strings.Fields(string(data[end+1:]))
			if len(fields) > 0 && fields[0] == "Z" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("grandchild process %d is still running", pid)
}
