package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezdbbackup/internal/backup"
	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/jobresolve"
	"github.com/ezgamehost/ezdbbackup/internal/logging"
	"github.com/ezgamehost/ezdbbackup/internal/stage"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
	"github.com/ezgamehost/ezdbbackup/internal/validation"
)

// Replacing the canonical config after local validation must fail before
// logger initialization, staging, dump execution, or network construction.
func TestBackupRejectsLoadedConfigInodeSwapBeforeSideEffects(t *testing.T) {
	root := secureCLITestDir(t)
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(shared, "trusted.yml")
	writeCLIConfig(t, trusted)
	path := filepath.Join(shared, "config.yml")
	if err := os.Symlink(trusted, path); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.LoadConfig = config.Load
	deps.Validator = validatorFunc(func(context.Context, *config.Config, []string, validation.Options) validation.Report {
		if err := os.Rename(trusted, trusted+".original"); err != nil {
			t.Fatal(err)
		}
		writeCLIConfig(t, trusted)
		return validation.Report{}
	})
	loggerCalls := 0
	deps.NewLogger = func(logging.Options) (logging.Sink, error) {
		loggerCalls++
		return discardSink{}, nil
	}
	runtime := &backupRuntime{}
	deps.NewBackup = runtime.newService
	deps.ExecutablePath = func() (string, error) { return "/usr/bin/true", nil }

	if code := Run(context.Background(), []string{"backup", "alpha", "--config", path}, deps); code != 2 {
		t.Fatalf("code = %d, stderr = %q; want source-safety exit 2", code, stderr.String())
	}
	if loggerCalls != 0 || len(runtime.dumps) != 0 {
		t.Fatalf("side effects = logger:%d dumps:%v, want none", loggerCalls, runtime.dumps)
	}
}

func TestBackupAcceptsFlagsBeforeAndAfterJob(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "flags before job", args: []string{"backup", "--config", "/custom/config.yml", "--debug", "alpha"}},
		{name: "flags after job", args: []string{"backup", "alpha", "--config", "/custom/config.yml", "--debug"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := fakeDependencies(&stdout, &stderr)
			cfg := cliTestConfig("alpha")
			var loadedPath string
			deps.LoadConfig = func(path string) (*config.Config, config.Findings) {
				loadedPath = path
				return cfg, nil
			}
			validator := &recordingValidator{}
			deps.Validator = validator
			var loggerOptions logging.Options
			deps.NewLogger = func(options logging.Options) (logging.Sink, error) {
				loggerOptions = options
				return discardSink{}, nil
			}
			runtime := &backupRuntime{}
			deps.NewBackup = runtime.newService

			if code := Run(context.Background(), tt.args, deps); code != 0 {
				t.Fatalf("code = %d, stderr = %q", code, stderr.String())
			}
			if loadedPath != "/custom/config.yml" {
				t.Fatalf("loaded path = %q", loadedPath)
			}
			if !loggerOptions.Debug {
				t.Fatal("logger debug = false, want true")
			}
			if !reflect.DeepEqual(validator.jobs, []string{"alpha"}) || validator.options.Connectivity || !validator.options.BackupExecution {
				t.Fatalf("validation = jobs:%v options:%+v", validator.jobs, validator.options)
			}
			if !reflect.DeepEqual(runtime.dumps, []string{"alpha"}) {
				t.Fatalf("dump jobs = %v", runtime.dumps)
			}
		})
	}
}

func TestBackupRejectsMissingConfigValueBeforeLoading(t *testing.T) {
	for _, args := range [][]string{
		{"backup", "--config", "--debug", "alpha"},
		{"backup", "alpha", "--config", "--debug"},
		{"backup", "--config", "--", "alpha"},
		{"backup", "alpha", "--config", "--"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := fakeDependencies(&stdout, &stderr)
			loads := 0
			deps.LoadConfig = func(string) (*config.Config, config.Findings) {
				loads++
				return cliTestConfig("alpha"), nil
			}
			deps.Validator = &recordingValidator{}
			deps.NewBackup = (&backupRuntime{}).newService

			if code := Run(context.Background(), args, deps); code != 2 {
				t.Fatalf("code = %d, want 2", code)
			}
			if loads != 0 {
				t.Fatalf("config loads = %d, want zero", loads)
			}
			if !strings.Contains(stderr.String(), "flag needs an argument") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestBackupAllowsExplicitEqualsForFlagLikeConfigValue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	var loaded string
	deps.LoadConfig = func(path string) (*config.Config, config.Findings) {
		loaded = path
		return cliTestConfig("alpha"), nil
	}
	deps.Validator = &recordingValidator{}
	deps.NewBackup = (&backupRuntime{}).newService

	if code := Run(context.Background(), []string{"backup", "alpha", "--config=--debug"}, deps); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.HasSuffix(loaded, "/--debug") {
		t.Fatalf("loaded path = %q, want explicit flag-like value", loaded)
	}
}

func TestBackupUsesDefaultConfigPathAndConfiguredDebug(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	cfg := cliTestConfig("alpha")
	cfg.Logging.Debug = true
	var loadedPath string
	deps.LoadConfig = func(path string) (*config.Config, config.Findings) {
		loadedPath = path
		return cfg, nil
	}
	deps.Validator = &recordingValidator{}
	var loggerOptions logging.Options
	deps.NewLogger = func(options logging.Options) (logging.Sink, error) {
		loggerOptions = options
		return discardSink{}, nil
	}
	deps.NewBackup = (&backupRuntime{}).newService

	if code := Run(context.Background(), []string{"backup", "alpha"}, deps); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if loadedPath != defaultConfigPath {
		t.Fatalf("loaded path = %q, want %q", loadedPath, defaultConfigPath)
	}
	if !loggerOptions.Debug {
		t.Fatal("configured debug was not mapped to logger")
	}
}

func TestBackupRejectsAmbiguousSelectionsBeforeSideEffects(t *testing.T) {
	for _, args := range [][]string{
		{"backup"},
		{"backup", "alpha", "bravo"},
		{"backup", "--all", "alpha"},
		{"backup", "alpha", "--all"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := fakeDependencies(&stdout, &stderr)
			loads, logs, backups := 0, 0, 0
			deps.LoadConfig = func(string) (*config.Config, config.Findings) {
				loads++
				return cliTestConfig("alpha", "bravo"), nil
			}
			deps.NewLogger = func(logging.Options) (logging.Sink, error) {
				logs++
				return discardSink{}, nil
			}
			deps.NewBackup = func(logging.Sink) *backup.Service {
				backups++
				return (&backupRuntime{}).newService(discardSink{})
			}

			if code := Run(context.Background(), args, deps); code != 2 {
				t.Fatalf("code = %d, want 2", code)
			}
			if loads != 0 || logs != 0 || backups != 0 {
				t.Fatalf("side effects = loads:%d logs:%d backups:%d", loads, logs, backups)
			}
		})
	}
}

func TestBackupRejectsMissingOrDisabledJobBeforeLoggerAndDump(t *testing.T) {
	for _, jobName := range []string{"missing", "disabled"} {
		t.Run(jobName, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := fakeDependencies(&stdout, &stderr)
			cfg := cliTestConfig("alpha", "disabled")
			disabled := cfg.Jobs["disabled"]
			disabled.Enabled = false
			cfg.Jobs["disabled"] = disabled
			deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cfg, nil }
			logs := 0
			deps.NewLogger = func(logging.Options) (logging.Sink, error) {
				logs++
				return discardSink{}, nil
			}
			runtime := &backupRuntime{}
			deps.NewBackup = runtime.newService

			if code := Run(context.Background(), []string{"backup", jobName}, deps); code != 2 {
				t.Fatalf("code = %d, stderr = %q", code, stderr.String())
			}
			if logs != 0 || len(runtime.dumps) != 0 {
				t.Fatalf("logger calls = %d, dumps = %v", logs, runtime.dumps)
			}
		})
	}
}

func TestBackupValidationFailurePreventsLoggerAndDump(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cliTestConfig("alpha"), nil }
	deps.Validator = &recordingValidator{report: validation.Report{Findings: []validation.Finding{{
		Severity: validation.SeverityError, Job: "alpha", Check: "dump_executable", Message: "unavailable",
	}}}}
	loggerCalls := 0
	deps.NewLogger = func(logging.Options) (logging.Sink, error) {
		loggerCalls++
		return discardSink{}, nil
	}
	runtime := &backupRuntime{}
	deps.NewBackup = runtime.newService

	if code := Run(context.Background(), []string{"backup", "alpha"}, deps); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if loggerCalls != 0 || len(runtime.dumps) != 0 {
		t.Fatalf("logger calls = %d, dumps = %v", loggerCalls, runtime.dumps)
	}
}

func TestBackupLoggerFailureExitsOneBeforeDump(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cliTestConfig("alpha"), nil }
	deps.Validator = &recordingValidator{}
	deps.NewLogger = func(logging.Options) (logging.Sink, error) { return nil, errors.New("permission denied") }
	runtime := &backupRuntime{}
	deps.NewBackup = runtime.newService

	if code := Run(context.Background(), []string{"backup", "alpha"}, deps); code != 1 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if len(runtime.dumps) != 0 || !strings.Contains(stderr.String(), "initialize logging") {
		t.Fatalf("dumps = %v, stderr = %q", runtime.dumps, stderr.String())
	}
}

func TestBackupLoggingConversionOverflowExitsTwoBeforeLogger(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	cfg := cliTestConfig("alpha")
	cfg.Logging.Rotation.MaxAgeDays = int(^uint64(0)>>1)/int(24*time.Hour) + 1
	deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cfg, nil }
	deps.Validator = &recordingValidator{}
	loggerCalls := 0
	deps.NewLogger = func(logging.Options) (logging.Sink, error) {
		loggerCalls++
		return discardSink{}, nil
	}

	if code := Run(context.Background(), []string{"backup", "alpha"}, deps); code != 2 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if loggerCalls != 0 {
		t.Fatalf("logger calls = %d, want zero", loggerCalls)
	}
	if !strings.Contains(stderr.String(), "logging") || !strings.Contains(stderr.String(), "overflow") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestBackupAllRunsLexicallyContinuesAndSummarizes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	cfg := cliTestConfig("zulu", "alpha", "disabled")
	disabled := cfg.Jobs["disabled"]
	disabled.Enabled = false
	cfg.Jobs["disabled"] = disabled
	deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cfg, nil }
	validator := &recordingValidator{}
	deps.Validator = validator
	runtime := &backupRuntime{dumpErrors: map[string]error{"alpha": errors.New("dump refused")}}
	deps.NewBackup = runtime.newService

	if code := Run(context.Background(), []string{"backup", "--all"}, deps); code != 1 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !reflect.DeepEqual(validator.jobs, []string{"alpha", "zulu"}) {
		t.Fatalf("validated jobs = %v", validator.jobs)
	}
	if !reflect.DeepEqual(runtime.dumps, []string{"alpha", "zulu"}) {
		t.Fatalf("dump jobs = %v, want continuation in lexical order", runtime.dumps)
	}
	out := stdout.String()
	alphaAt, zuluAt := strings.Index(out, "alpha: failed at dump_execution:"), strings.Index(out, "zulu: upload complete ")
	if alphaAt < 0 || zuluAt <= alphaAt || !strings.Contains(out, "backup summary: 1 succeeded, 1 failed\n") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestBackupAllWithNoEnabledJobsHasNoValidationOrBackupEffects(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	cfg := cliTestConfig("disabled")
	disabled := cfg.Jobs["disabled"]
	disabled.Enabled = false
	disabled.MySQL.Host = ""
	cfg.Jobs["disabled"] = disabled
	deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cfg, nil }
	validatorCalls, loggerCalls, backupCalls := 0, 0, 0
	deps.Validator = checkerFunc(func(context.Context, *config.Config, []string, validation.Options) validation.Report {
		validatorCalls++
		return validation.Report{}
	})
	deps.NewLogger = func(logging.Options) (logging.Sink, error) {
		loggerCalls++
		return discardSink{}, nil
	}
	deps.NewBackup = func(logging.Sink) *backup.Service {
		backupCalls++
		return (&backupRuntime{}).newService(discardSink{})
	}

	if code := Run(context.Background(), []string{"backup", "--all"}, deps); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if validatorCalls != 0 || loggerCalls != 0 || backupCalls != 0 {
		t.Fatalf("effects = validator:%d logger:%d backup:%d", validatorCalls, loggerCalls, backupCalls)
	}
	if got := stdout.String(); got != "backup summary: 0 succeeded, 0 failed\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestBackupAllWithNoEnabledJobsRejectsGlobalConfigurationErrorsWithoutEffects(t *testing.T) {
	maxSizeMB := int(^uint64(0)>>1) / (1024 * 1024)
	maxAgeDays := int(^uint64(0)>>1) / int(24*time.Hour)
	for _, tt := range []struct {
		name     string
		edit     func(*config.Config)
		wantPath string
	}{
		{name: "version", edit: func(cfg *config.Config) { cfg.Version = 2 }, wantPath: "version"},
		{name: "default dump path", edit: func(cfg *config.Config) { cfg.Defaults.DumpBinary = "mysqldump" }, wantPath: "defaults.dump_binary"},
		{name: "default temp path", edit: func(cfg *config.Config) { cfg.Defaults.TempDir = "tmp" }, wantPath: "defaults.temp_dir"},
		{name: "log path", edit: func(cfg *config.Config) { cfg.Logging.Directory = "logs" }, wantPath: "logging.directory"},
		{name: "rotation size zero", edit: func(cfg *config.Config) { cfg.Logging.Rotation.MaxSizeMB = 0 }, wantPath: "logging.rotation.max_size_mb"},
		{name: "rotation files zero", edit: func(cfg *config.Config) { cfg.Logging.Rotation.MaxFiles = 0 }, wantPath: "logging.rotation.max_files"},
		{name: "rotation age zero", edit: func(cfg *config.Config) { cfg.Logging.Rotation.MaxAgeDays = 0 }, wantPath: "logging.rotation.max_age_days"},
		{name: "rotation size", edit: func(cfg *config.Config) { cfg.Logging.Rotation.MaxSizeMB = maxSizeMB + 1 }, wantPath: "logging.rotation.max_size_mb"},
		{name: "rotation age", edit: func(cfg *config.Config) { cfg.Logging.Rotation.MaxAgeDays = maxAgeDays + 1 }, wantPath: "logging.rotation.max_age_days"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := fakeDependencies(&stdout, &stderr)
			cfg := cliTestConfig("disabled")
			disabled := cfg.Jobs["disabled"]
			disabled.Enabled = false
			cfg.Jobs["disabled"] = disabled
			tt.edit(cfg)
			deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cfg, nil }
			validatorCalls, loggerCalls, backupCalls := 0, 0, 0
			deps.Validator = checkerFunc(func(context.Context, *config.Config, []string, validation.Options) validation.Report {
				validatorCalls++
				return validation.Report{}
			})
			deps.NewLogger = func(logging.Options) (logging.Sink, error) {
				loggerCalls++
				return discardSink{}, nil
			}
			deps.NewBackup = func(logging.Sink) *backup.Service {
				backupCalls++
				return (&backupRuntime{}).newService(discardSink{})
			}

			if code := Run(context.Background(), []string{"backup", "--all"}, deps); code != 2 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
			if validatorCalls != 0 || loggerCalls != 0 || backupCalls != 0 {
				t.Fatalf("effects = validator:%d logger:%d backup:%d", validatorCalls, loggerCalls, backupCalls)
			}
			if !strings.Contains(stderr.String(), tt.wantPath) {
				t.Fatalf("stderr = %q, want global finding %q", stderr.String(), tt.wantPath)
			}
			if strings.Contains(stdout.String(), "backup summary:") {
				t.Fatalf("stdout = %q, want no success summary", stdout.String())
			}
		})
	}
}

type recordingValidator struct {
	jobs    []string
	options validation.Options
	report  validation.Report
}

type checkerFunc func(context.Context, *config.Config, []string, validation.Options) validation.Report

func (f checkerFunc) Check(ctx context.Context, cfg *config.Config, jobs []string, options validation.Options) validation.Report {
	return f(ctx, cfg, jobs, options)
}

func (v *recordingValidator) Check(_ context.Context, _ *config.Config, jobs []string, options validation.Options) validation.Report {
	v.jobs = append([]string(nil), jobs...)
	v.options = options
	return v.report
}

func cliTestConfig(names ...string) *config.Config {
	cfg := &config.Config{
		Version:  1,
		Defaults: config.Defaults{DumpBinary: "/usr/bin/mysqldump", TempDir: "/tmp"},
		Logging: config.LoggingConfig{
			Directory: "/var/log/ezdbbackup",
			Rotation:  config.RotationConfig{MaxSizeMB: 100, MaxFiles: 7, MaxAgeDays: 30, Compress: true},
		},
		Jobs: make(map[string]config.JobConfig, len(names)),
	}
	for _, name := range names {
		cfg.Jobs[name] = config.JobConfig{
			Enabled: true, Schedule: "0 2 * * *", RunAs: "root", DumpBinary: "/usr/bin/mysqldump", TempDir: "/tmp",
			MySQL: config.MySQLConfig{Host: name, Port: 3306, User: "backup", Databases: config.DatabaseSelection{All: true}},
			S3:    config.S3Config{Bucket: "backups", Region: "us-east-1"},
		}
	}
	return cfg
}

type backupRuntime struct {
	dumps      []string
	dumpErrors map[string]error
}

func (r *backupRuntime) newService(sink logging.Sink) *backup.Service {
	return &backup.Service{
		Resolve: runtimeResolver{},
		Dump:    runtimeDump{runtime: r},
		Stager:  runtimeStager{},
		Stores:  runtimeFactory{},
		Log:     sink,
	}
}

type runtimeResolver struct{}

func (runtimeResolver) Dump(job config.JobConfig) (dump.Request, error) {
	return dump.Request{Host: job.MySQL.Host}, nil
}

func (runtimeResolver) Storage(config.JobConfig) (storage.Options, error) {
	return storage.Options{}, nil
}

var _ jobresolve.OptionsResolver = runtimeResolver{}

type runtimeDump struct{ runtime *backupRuntime }

func (d runtimeDump) Run(_ context.Context, request dump.Request, writer io.Writer) error {
	d.runtime.dumps = append(d.runtime.dumps, request.Host)
	if err := d.runtime.dumpErrors[request.Host]; err != nil {
		return err
	}
	_, err := io.WriteString(writer, "dump")
	return err
}

func (runtimeDump) Probe(context.Context, dump.Request) error { return nil }

type runtimeStager struct{}

func (runtimeStager) Stage(ctx context.Context, _ string, write func(io.Writer) error) (stage.Artifact, error) {
	if err := write(io.Discard); err != nil {
		return stage.Artifact{}, err
	}
	return stage.Artifact{Path: "/tmp/artifact.sql.gz", Size: 42}, ctx.Err()
}

func (runtimeStager) Remove(stage.Artifact) error { return nil }

func (runtimeStager) Open(stage.Artifact) (*os.File, error) { return os.Open(os.DevNull) }

type runtimeFactory struct{}

func (runtimeFactory) New(context.Context, storage.Options) (storage.Store, error) {
	return runtimeStore{}, nil
}

type runtimeStore struct{}

func (runtimeStore) UploadFile(context.Context, string, string, io.ReaderAt, int64) (storage.UploadResult, error) {
	return storage.UploadResult{}, nil
}

func (runtimeStore) Probe(context.Context, string) error { return nil }
