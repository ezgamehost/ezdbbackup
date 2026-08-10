package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/logging"
)

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.Version = "v1.2.3"

	if code := Run(context.Background(), []string{"version"}, deps); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if got := stdout.String(); got != "ezdbbackup v1.2.3\n" {
		t.Fatalf("stdout = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestUsageAndUnknownCommandExitTwoWithText(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "empty", want: "Usage:"},
		{name: "unknown", args: []string{"frobnicate"}, want: "unknown command"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), tt.args, fakeDependencies(&stdout, &stderr)); code != 2 {
				t.Fatalf("code = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.want)
			}
			if strings.Contains(stderr.String(), "{\"") {
				t.Fatalf("stderr contains JSON: %q", stderr.String())
			}
		})
	}
}

func TestMalformedCommandFlagsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"backup", "--bogus"},
		{"validate", "--bogus"},
		{"cron", "install", "--bogus"},
		{"cron", "show", "--bogus"},
		{"cron", "remove", "--bogus"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), args, fakeDependencies(&stdout, &stderr)); code != 2 {
				t.Fatalf("Run(%q) code = %d, want 2", args, code)
			}
			if stderr.Len() == 0 || strings.Contains(stderr.String(), "{\"") {
				t.Fatalf("stderr = %q, want concise text", stderr.String())
			}
		})
	}
}

func fakeDependencies(stdout, stderr *bytes.Buffer) Dependencies {
	return Dependencies{
		Stdout:  stdout,
		Stderr:  stderr,
		Version: "dev",
		LoadConfig: func(string) (*config.Config, config.Findings) {
			return &config.Config{}, nil
		},
		NewLogger: func(logging.Options) (logging.Sink, error) {
			return discardSink{}, nil
		},
		ExecutablePath: func() (string, error) { return "/usr/bin/ezdbbackup", nil },
	}
}

type discardSink struct{}

func (discardSink) Write(logging.Event) error { return nil }
