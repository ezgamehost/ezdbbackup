package dump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
		"--host=db.internal", "--port=3307", "--user=backup", "--databases", "app",
	}; !slices.Equal(got, want) {
		t.Fatalf("arguments = %#v, want %#v", got, want)
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
		"--host=db.internal", "--port=3307", "--user=backup", "--databases", "app",
		"--no-data", "--no-create-info", "--skip-triggers",
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
	binary := writeFixture(t, "wait", filepath.Join(t.TempDir(), "password"), filepath.Join(t.TempDir(), "args"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := (ExecRunner{}).Run(ctx, Request{
		Binary: binary, Host: "db.internal", Port: 3307, User: "backup", Databases: []string{"app"},
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("Run() error = nil, want cancellation")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context deadline exceeded", err)
	}
}

func writeFixture(t *testing.T, mode, passwordPath, argsPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mysqldump-fixture")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"${MYSQL_PWD}\" > " + shellQuote(passwordPath) + "\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\n" +
		"case " + shellQuote(mode) + " in\n" +
		"success) printf 'dump contents\\n' ;;\n" +
		"failure) printf 'fixture failure\\n' >&2; exit 23 ;;\n" +
		"passwordfailure) printf '%s' \"${MYSQL_PWD}\" >&2; exit 23 ;;\n" +
		"loudsuccess) i=0; while [ \"$i\" -lt 100 ]; do printf x >&2; i=$((i + 1)); done; printf 'dump contents\\n' ;;\n" +
		"wait) exec sleep 10 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeStderrFailureFixture(t *testing.T, writes []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mysqldump-stderr-fixture")
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
