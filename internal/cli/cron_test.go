package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/cron"
	"github.com/ezgamehost/ezdbbackup/internal/validation"
)

// Rendering the originally requested sticky-directory symlink here would let
// its owner redirect every future root cron invocation after installation.
func TestCronInstallUsesLoadedCanonicalConfigPathAcrossSymlinkSwap(t *testing.T) {
	root := secureCLITestDir(t)
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(shared, "trusted.yml")
	replacement := filepath.Join(shared, "replacement.yml")
	writeCLIConfig(t, trusted)
	writeCLIConfig(t, replacement)
	link := filepath.Join(shared, "config.yml")
	if err := os.Symlink(trusted, link); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.LoadConfig = config.Load
	deps.Validator = validatorFunc(func(context.Context, *config.Config, []string, validation.Options) validation.Report {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(replacement, link); err != nil {
			t.Fatal(err)
		}
		return validation.Report{}
	})
	manager := &recordingCron{}
	deps.Cron = manager
	deps.ExecutablePath = func() (string, error) { return "/usr/bin/true", nil }

	if code := Run(context.Background(), []string{"cron", "install", "--config", link}, deps); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(string(manager.installed), "--config '"+trusted+"'") {
		t.Fatalf("installed schedule = %q, want canonical config %q", manager.installed, trusted)
	}
	if strings.Contains(string(manager.installed), "--config '"+link+"'") {
		t.Fatalf("installed schedule retains replaceable symlink %q", link)
	}
}

func TestCronInstallUsesCanonicalExecutableTarget(t *testing.T) {
	root := secureCLITestDir(t)
	target := filepath.Join(root, "ezdbbackup-target")
	if err := os.WriteFile(target, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "ezdbbackup")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cliTestConfig("alpha"), nil }
	deps.Validator = &recordingValidator{}
	manager := &recordingCron{}
	deps.Cron = manager
	deps.ExecutablePath = func() (string, error) { return link, nil }

	if code := Run(context.Background(), []string{"cron", "install"}, deps); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(string(manager.installed), "'"+target+"' backup") {
		t.Fatalf("installed schedule = %q, want canonical executable %q", manager.installed, target)
	}
}

// A canonical inode replacement after validation must be detected before the
// cron manager can mutate /etc/cron.d.
func TestCronInstallRejectsLoadedConfigInodeSwapBeforeMutation(t *testing.T) {
	root := secureCLITestDir(t)
	path := filepath.Join(root, "config.yml")
	writeCLIConfig(t, path)

	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.LoadConfig = config.Load
	deps.Validator = validatorFunc(func(context.Context, *config.Config, []string, validation.Options) validation.Report {
		if err := os.Rename(path, path+".original"); err != nil {
			t.Fatal(err)
		}
		writeCLIConfig(t, path)
		return validation.Report{}
	})
	manager := &recordingCron{}
	deps.Cron = manager
	deps.ExecutablePath = func() (string, error) { return "/usr/bin/true", nil }

	if code := Run(context.Background(), []string{"cron", "install", "--config", path}, deps); code != 2 {
		t.Fatalf("code = %d, stderr = %q; want source-safety exit 2", code, stderr.String())
	}
	if manager.installCalls != 0 {
		t.Fatalf("cron install calls = %d, want zero", manager.installCalls)
	}
}

type validatorFunc func(context.Context, *config.Config, []string, validation.Options) validation.Report

func (fn validatorFunc) Check(ctx context.Context, cfg *config.Config, jobs []string, options validation.Options) validation.Report {
	return fn(ctx, cfg, jobs, options)
}

func secureCLITestDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func writeCLIConfig(t *testing.T, path string) {
	t.Helper()
	contents := "version: 1\n" +
		"jobs:\n" +
		"  alpha:\n" +
		"    enabled: true\n" +
		"    schedule: '0 2 * * *'\n" +
		"    run_as: root\n" +
		"    mysql:\n" +
		"      host: db.internal\n" +
		"      user: backup\n" +
		"      databases: all\n" +
		"    s3:\n" +
		"      bucket: backups\n" +
		"      region: us-east-1\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCronInstallValidatesAllJobsThenWritesExactSchedule(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	cfg := cliTestConfig("zulu", "alpha", "disabled")
	disabled := cfg.Jobs["disabled"]
	disabled.Enabled = false
	cfg.Jobs["disabled"] = disabled
	deps.LoadConfig = func(path string) (*config.Config, config.Findings) {
		if path != "/custom/config.yml" {
			t.Fatalf("LoadConfig path = %q", path)
		}
		return cfg, nil
	}
	validator := &recordingValidator{}
	deps.Validator = validator
	manager := &recordingCron{}
	deps.Cron = manager
	deps.ExecutablePath = func() (string, error) { return "/opt/ez db/ezdbbackup", nil }

	if code := Run(context.Background(), []string{"cron", "install", "--config", "/custom/config.yml"}, deps); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if validator.jobs != nil || validator.options.Connectivity {
		t.Fatalf("validation = jobs:%#v options:%+v, want full local validation", validator.jobs, validator.options)
	}
	want := "# Generated by ezdbbackup. DO NOT EDIT.\n" +
		cron.OwnershipMarker + "\n" +
		"SHELL=/bin/sh\n" +
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n\n" +
		"0 2 * * * root '/opt/ez db/ezdbbackup' backup 'alpha' --config '/custom/config.yml'\n" +
		"0 2 * * * root '/opt/ez db/ezdbbackup' backup 'zulu' --config '/custom/config.yml'\n"
	if got := string(manager.installed); got != want {
		t.Fatalf("installed schedule =\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(strings.ToLower(string(manager.installed)), "logrotate") {
		t.Fatalf("schedule contains logrotate: %q", manager.installed)
	}
}

func TestCronInstallValidationFailureDoesNotMutate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cliTestConfig("alpha"), nil }
	deps.Validator = &recordingValidator{report: validation.Report{Findings: []validation.Finding{{
		Severity: validation.SeverityError, Job: "alpha", Check: "schedule", Message: "invalid",
	}}}}
	manager := &recordingCron{installed: []byte("unchanged")}
	deps.Cron = manager

	if code := Run(context.Background(), []string{"cron", "install"}, deps); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if manager.installCalls != 0 || string(manager.installed) != "unchanged" {
		t.Fatalf("install calls = %d, content = %q", manager.installCalls, manager.installed)
	}
}

func TestCronInstallManagerFailureExitsThree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cliTestConfig("alpha"), nil }
	deps.Validator = &recordingValidator{}
	deps.Cron = &recordingCron{installErr: errors.New("permission denied")}

	if code := Run(context.Background(), []string{"cron", "install"}, deps); code != 3 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCronShowPrintsExactManagedFileWithoutLoadingConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	loads := 0
	deps.LoadConfig = func(string) (*config.Config, config.Findings) {
		loads++
		return nil, nil
	}
	want := []byte("# exact\nline without forced newline")
	manager := &recordingCron{shown: want}
	deps.Cron = manager

	if code := Run(context.Background(), []string{"cron", "show"}, deps); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !bytes.Equal(stdout.Bytes(), want) || loads != 0 {
		t.Fatalf("stdout = %q, loads = %d", stdout.String(), loads)
	}
}

func TestCronRemoveIsIdempotentAndDoesNotLoadConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	loads := 0
	deps.LoadConfig = func(string) (*config.Config, config.Findings) {
		loads++
		return nil, nil
	}
	manager := &recordingCron{}
	deps.Cron = manager

	for range 2 {
		if code := Run(context.Background(), []string{"cron", "remove"}, deps); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	}
	if manager.removeCalls != 2 || loads != 0 {
		t.Fatalf("remove calls = %d, loads = %d", manager.removeCalls, loads)
	}
}

func TestCronShowAndRemoveFailuresExitThree(t *testing.T) {
	for _, command := range []string{"show", "remove"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := fakeDependencies(&stdout, &stderr)
			manager := &recordingCron{}
			if command == "show" {
				manager.showErr = errors.New("not owned")
			} else {
				manager.removeErr = errors.New("not owned")
			}
			deps.Cron = manager
			if code := Run(context.Background(), []string{"cron", command}, deps); code != 3 {
				t.Fatalf("code = %d, want 3", code)
			}
		})
	}
}

type recordingCron struct {
	installed    []byte
	shown        []byte
	installErr   error
	showErr      error
	removeErr    error
	installCalls int
	removeCalls  int
}

func (c *recordingCron) Install(content []byte) error {
	c.installCalls++
	if c.installErr == nil {
		c.installed = append([]byte(nil), content...)
	}
	return c.installErr
}

func (c *recordingCron) Show() ([]byte, error) {
	return append([]byte(nil), c.shown...), c.showErr
}

func (c *recordingCron) Remove() error {
	c.removeCalls++
	return c.removeErr
}

var _ CronService = (*recordingCron)(nil)

func TestCronInstallUsesEffectiveAbsoluteConfigPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.LoadConfig = func(string) (*config.Config, config.Findings) { return cliTestConfig("alpha"), nil }
	validator := &recordingValidator{}
	deps.Validator = validator
	manager := &recordingCron{}
	deps.Cron = manager

	if code := Run(context.Background(), []string{"cron", "install", "--config", "relative.yml"}, deps); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.HasSuffix(validator.options.ConfigPath, "/relative.yml") || !strings.Contains(string(manager.installed), "--config '"+validator.options.ConfigPath+"'") {
		t.Fatalf("validation options = %+v, schedule = %q", validator.options, manager.installed)
	}
	if reflect.DeepEqual(manager.installed, []byte(nil)) {
		t.Fatal("install was not called")
	}
}
