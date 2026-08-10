package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/validation"
)

func TestCLIEncodesControlledValuesAsSingleBoundedLines(t *testing.T) {
	malicious := "value\nnext\r\x1b]0;owned\a\u202e" + strings.Repeat("x", 10000)
	tests := []struct {
		name string
		run  func(*Dependencies) []string
	}{
		{
			name: "version",
			run: func(deps *Dependencies) []string {
				deps.Version = malicious
				return []string{"version"}
			},
		},
		{
			name: "unknown command",
			run:  func(*Dependencies) []string { return []string{malicious} },
		},
		{
			name: "config finding",
			run: func(deps *Dependencies) []string {
				deps.LoadConfig = func(string) (*config.Config, config.Findings) {
					return nil, config.Findings{{Path: malicious, Message: malicious}}
				}
				return []string{"validate"}
			},
		},
		{
			name: "validation cause",
			run: func(deps *Dependencies) []string {
				deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cliTestConfig("alpha"), nil }
				deps.Validator = &recordingValidator{report: validation.Report{Findings: []validation.Finding{{
					Severity: validation.SeverityError, Job: malicious, Check: malicious, Message: malicious, Cause: errors.New(malicious),
				}}}}
				return []string{"validate"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := fakeDependencies(&stdout, &stderr)
			args := tt.run(&deps)
			_ = Run(context.Background(), args, deps)
			combined := stdout.String() + stderr.String()
			if strings.ContainsRune(combined, '\x1b') || strings.ContainsRune(combined, '\r') || strings.Contains(combined, "\u202e") {
				t.Fatalf("output contains unsafe terminal controls: %q", combined)
			}
			if strings.Contains(combined, "value\nnext") {
				t.Fatalf("controlled newline created a second line: %q", combined)
			}
			if len(combined) > 9000 {
				t.Fatalf("output length = %d, want bounded controlled values", len(combined))
			}
			if !strings.Contains(combined, `\n`) || !strings.Contains(combined, `\x1b`) {
				t.Fatalf("output = %q, want visible escapes", combined)
			}
		})
	}
}

func TestCronShowEncodesUntrustedManagedContent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.Cron = &recordingCron{shown: []byte("# managed\njob\r\x1b]8;;owned\n")}

	if code := Run(context.Background(), []string{"cron", "show"}, deps); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "# managed\njob\\r\\x1b]8;;owned\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestBackupProgressEncodesMaliciousDumpStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cliTestConfig("alpha"), nil }
	deps.Validator = &recordingValidator{}
	runtime := &backupRuntime{dumpErrors: map[string]error{
		"alpha": errors.New("mysqldump: disk full\n\x1b]8;;https://evil\a click\r\u202e"),
	}}
	deps.NewBackup = runtime.newService

	if code := Run(context.Background(), []string{"backup", "alpha"}, deps); code != 1 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	if strings.ContainsRune(output, '\x1b') || strings.ContainsRune(output, '\r') || strings.Contains(output, "disk full\n\x1b") {
		t.Fatalf("backup output contains active controls: %q", output)
	}
	for _, escaped := range []string{`\n`, `\r`, `\x1b`, `\x07`, `\u202e`} {
		if !strings.Contains(output, escaped) {
			t.Fatalf("backup output = %q, want %s", output, escaped)
		}
	}
}
