package cli

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/logging"
	"github.com/ezgamehost/ezdbbackup/internal/validation"
)

func TestValidateDefaultsToAllJobsIncludingDisabledWithoutLogging(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	cfg := cliTestConfig("zulu", "alpha")
	disabled := cfg.Jobs["zulu"]
	disabled.Enabled = false
	cfg.Jobs["zulu"] = disabled
	deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cfg, nil }
	validator := &recordingValidator{}
	deps.Validator = validator
	loggerCalls := 0
	deps.NewLogger = func(logging.Options) (logging.Sink, error) {
		loggerCalls++
		return discardSink{}, nil
	}

	if code := Run(context.Background(), []string{"validate", "--debug"}, deps); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if validator.jobs != nil {
		t.Fatalf("selected jobs = %#v, want nil for validator default-all semantics", validator.jobs)
	}
	if loggerCalls != 0 {
		t.Fatalf("logger calls = %d, want zero", loggerCalls)
	}
	if stdout.String() != "validation succeeded\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestValidateAcceptsFlagsBeforeAndAfterExplicitJob(t *testing.T) {
	for _, args := range [][]string{
		{"validate", "--config", "/custom/config.yml", "--connectivity", "alpha"},
		{"validate", "alpha", "--connectivity", "--config", "/custom/config.yml"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := fakeDependencies(&stdout, &stderr)
			var loaded string
			deps.LoadConfig = func(path string) (*config.Config, config.Findings) {
				loaded = path
				return cliTestConfig("alpha", "bravo"), nil
			}
			validator := &recordingValidator{}
			deps.Validator = validator

			if code := Run(context.Background(), args, deps); code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, stderr.String())
			}
			if loaded != "/custom/config.yml" || !reflect.DeepEqual(validator.jobs, []string{"alpha"}) {
				t.Fatalf("loaded = %q, jobs = %v", loaded, validator.jobs)
			}
			if !validator.options.Connectivity || validator.options.ConfigPath != "/custom/config.yml" {
				t.Fatalf("validation options = %+v", validator.options)
			}
		})
	}
}

func TestValidateRejectsAllWithJobAndMultipleJobsBeforeLoading(t *testing.T) {
	for _, args := range [][]string{
		{"validate", "--all", "alpha"},
		{"validate", "alpha", "--all"},
		{"validate", "alpha", "bravo"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := fakeDependencies(&stdout, &stderr)
			loads := 0
			deps.LoadConfig = func(string) (*config.Config, config.Findings) {
				loads++
				return cliTestConfig("alpha"), nil
			}
			if code := Run(context.Background(), args, deps); code != 2 {
				t.Fatalf("code = %d, want 2", code)
			}
			if loads != 0 {
				t.Fatalf("config loads = %d, want zero", loads)
			}
		})
	}
}

func TestValidateWarningsExitZeroAndErrorsExitTwo(t *testing.T) {
	for _, tt := range []struct {
		name       string
		finding    validation.Finding
		wantCode   int
		wantStream string
	}{
		{
			name:       "warning",
			finding:    validation.Finding{Severity: validation.SeverityWarning, Job: "alpha", Check: "endpoint", Message: "plain HTTP configured"},
			wantCode:   0,
			wantStream: "stdout",
		},
		{
			name:       "error",
			finding:    validation.Finding{Severity: validation.SeverityError, Job: "alpha", Check: "dump_executable", Message: "not executable"},
			wantCode:   2,
			wantStream: "stderr",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := fakeDependencies(&stdout, &stderr)
			deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cliTestConfig("alpha"), nil }
			deps.Validator = &recordingValidator{report: validation.Report{Findings: []validation.Finding{tt.finding}}}

			if code := Run(context.Background(), []string{"validate"}, deps); code != tt.wantCode {
				t.Fatalf("code = %d, want %d", code, tt.wantCode)
			}
			output := stdout.String()
			if tt.wantStream == "stderr" {
				output = stderr.String()
			}
			if !strings.Contains(output, tt.finding.Message) || strings.Contains(output, "{\"") {
				t.Fatalf("%s = %q, want concise finding", tt.wantStream, output)
			}
		})
	}
}
