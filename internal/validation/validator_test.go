package validation

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
)

func TestValidatorChecksEveryJobIncludingDisabledInLexicalOrder(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Jobs["zulu"] = validValidationJob("zulu", false)
	cfg.Jobs["alpha"] = validValidationJob("alpha", true)
	env := &fakeEnvironment{}
	remote := &fakeRemoteDependencies{}

	report := newValidator(env, remote).Check(context.Background(), cfg, nil, Options{
		BinaryPath: "/usr/local/bin/ezdbbackup",
		ConfigPath: "/etc/ezdbbackup/config.yml",
	})

	if report.HasErrors() {
		t.Fatalf("Check() report = %#v, want no errors", report.Findings)
	}
	want := []string{
		"cron:/usr/local/bin/ezdbbackup", "cron:/etc/ezdbbackup/config.yml",
		"user:alpha-user", "executable:/dump/alpha:alpha-user", "writable:/tmp/alpha:alpha-user", "writable:/logs:alpha-user",
		"secret:/secrets/alpha-mysql:alpha-user", "secret:/secrets/alpha-access:alpha-user", "secret:/secrets/alpha-secret:alpha-user", "secret:/secrets/alpha-session:alpha-user",
		"user:zulu-user", "executable:/dump/zulu:zulu-user", "writable:/tmp/zulu:zulu-user", "writable:/logs:zulu-user",
		"secret:/secrets/zulu-mysql:zulu-user", "secret:/secrets/zulu-access:zulu-user", "secret:/secrets/zulu-secret:zulu-user", "secret:/secrets/zulu-session:zulu-user",
	}
	if !reflect.DeepEqual(env.calls, want) {
		t.Fatalf("environment calls = %#v, want %#v", env.calls, want)
	}
	if len(remote.calls) != 0 {
		t.Fatalf("remote calls = %v, want none without connectivity", remote.calls)
	}
}

func TestValidatorSelectedJobsLimitConfigurationAndEnvironmentChecks(t *testing.T) {
	cfg := validValidationConfig()
	alpha := validValidationJob("alpha", true)
	alpha.MySQL.Host = ""
	cfg.Jobs["alpha"] = alpha
	cfg.Jobs["bravo"] = validValidationJob("bravo", true)
	env := &fakeEnvironment{}

	report := newValidator(env, &fakeRemoteDependencies{}).Check(context.Background(), cfg, []string{"bravo"}, Options{
		BinaryPath: "/bin/ezdbbackup",
		ConfigPath: "/etc/ezdbbackup/config.yml",
	})

	if report.HasErrors() {
		t.Fatalf("Check(selected) findings = %#v, want invalid unselected job ignored", report.Findings)
	}
	for _, call := range env.calls {
		if strings.Contains(call, "alpha") {
			t.Fatalf("unselected alpha environment call = %q", call)
		}
	}
}

func TestValidatorReportsUnknownSelectedJob(t *testing.T) {
	report := newValidator(&fakeEnvironment{}, &fakeRemoteDependencies{}).Check(
		context.Background(), validValidationConfig(), []string{"missing"}, Options{
			BinaryPath: "/bin/ezdbbackup",
			ConfigPath: "/etc/ezdbbackup/config.yml",
		},
	)
	if !report.HasErrors() || !hasFinding(report, "missing", "selection", "not configured") {
		t.Fatalf("Check(missing) findings = %#v, want selection error", report.Findings)
	}
}

func TestValidatorLocalOnlyReturnsFindingsWithoutResolvingOrCallingRemote(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Jobs["alpha"] = validValidationJob("alpha", true)
	env := &fakeEnvironment{errors: map[string]error{
		"writable:/tmp/alpha:alpha-user": errors.New("read-only filesystem"),
	}}
	remote := &fakeRemoteDependencies{}

	report := newValidator(env, remote).Check(context.Background(), cfg, nil, Options{
		BinaryPath: "/bin/ezdbbackup",
		ConfigPath: "/etc/ezdbbackup/config.yml",
	})

	if !hasFinding(report, "alpha", "temp_directory", "not writable") {
		t.Fatalf("Check() findings = %#v, want temp-directory finding", report.Findings)
	}
	if len(remote.calls) != 0 {
		t.Fatalf("local validation remote calls = %v, want zero", remote.calls)
	}
}

func TestValidatorConnectivityChecksMySQLAndS3SeparatelyAndContinues(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Jobs["bravo"] = validValidationJob("bravo", true)
	cfg.Jobs["alpha"] = validValidationJob("alpha", true)
	dumpErr := errors.New("mysql refused connection")
	bucketErr := errors.New("access denied")
	remote := &fakeRemoteDependencies{errors: map[string]error{
		"dump-probe:alpha":         dumpErr,
		"store-probe:alpha-bucket": bucketErr,
	}}

	report := newValidator(&fakeEnvironment{}, remote).Check(context.Background(), cfg, nil, Options{
		Connectivity: true,
		BinaryPath:   "/bin/ezdbbackup",
		ConfigPath:   "/etc/ezdbbackup/config.yml",
	})

	wantCalls := []string{
		"resolve-dump:alpha", "dump-probe:alpha", "resolve-storage:alpha", "new-store:alpha", "store-probe:alpha-bucket",
		"resolve-dump:bravo", "dump-probe:bravo", "resolve-storage:bravo", "new-store:bravo", "store-probe:bravo-bucket",
	}
	if !reflect.DeepEqual(remote.calls, wantCalls) {
		t.Fatalf("connectivity calls = %#v, want %#v", remote.calls, wantCalls)
	}
	if !hasFinding(report, "alpha", "mysql_connectivity", "connection probe failed") {
		t.Fatalf("findings = %#v, want MySQL finding", report.Findings)
	}
	if !hasFinding(report, "alpha", "s3_connectivity", "bucket inspection may be denied even when uploads are allowed") {
		t.Fatalf("findings = %#v, want S3 policy caveat", report.Findings)
	}
	if !errors.Is(findFinding(report, "alpha", "mysql_connectivity"), dumpErr) {
		t.Fatal("MySQL finding does not wrap probe error")
	}
	if !errors.Is(findFinding(report, "alpha", "s3_connectivity"), bucketErr) {
		t.Fatal("S3 finding does not wrap bucket error")
	}
}

func TestValidatorCompletesAllLocalChecksBeforeConnectivity(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Jobs["bravo"] = validValidationJob("bravo", true)
	cfg.Jobs["alpha"] = validValidationJob("alpha", true)
	events := make([]string, 0)
	env := &fakeEnvironment{events: &events}
	remote := &fakeRemoteDependencies{events: &events}

	newValidator(env, remote).Check(context.Background(), cfg, nil, Options{
		Connectivity: true,
		BinaryPath:   "/bin/ezdbbackup",
		ConfigPath:   "/etc/ezdbbackup/config.yml",
	})

	lastLocal := -1
	firstRemote := -1
	for i, event := range events {
		if strings.HasPrefix(event, "local:") {
			lastLocal = i
		}
		if firstRemote < 0 && strings.HasPrefix(event, "remote:") {
			firstRemote = i
		}
	}
	if firstRemote < 0 || lastLocal > firstRemote {
		t.Fatalf("event order = %#v, want every local check before first remote check", events)
	}
}

func TestValidatorSkipsOnlyRemoteCheckWhoseLocalPrerequisiteFailed(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Jobs["alpha"] = validValidationJob("alpha", true)
	cfg.Jobs["bravo"] = validValidationJob("bravo", true)
	env := &fakeEnvironment{errors: map[string]error{
		"executable:/dump/alpha:alpha-user":       errors.New("not executable"),
		"secret:/secrets/bravo-access:bravo-user": errors.New("access credential unreadable"),
	}}
	remote := &fakeRemoteDependencies{}

	report := newValidator(env, remote).Check(context.Background(), cfg, nil, Options{
		Connectivity: true,
		BinaryPath:   "/bin/ezdbbackup",
		ConfigPath:   "/etc/ezdbbackup/config.yml",
	})

	wantCalls := []string{
		"resolve-storage:alpha", "new-store:alpha", "store-probe:alpha-bucket",
		"resolve-dump:bravo", "dump-probe:bravo",
	}
	if !reflect.DeepEqual(remote.calls, wantCalls) {
		t.Fatalf("remote calls = %#v, want %#v", remote.calls, wantCalls)
	}
	if !hasFinding(report, "alpha", "mysql_connectivity", "skipped because local MySQL prerequisites failed") {
		t.Fatalf("findings = %#v, want precise MySQL skipped finding", report.Findings)
	}
	if !hasFinding(report, "bravo", "s3_connectivity", "skipped because local S3 prerequisites failed") {
		t.Fatalf("findings = %#v, want precise S3 skipped finding", report.Findings)
	}
}

func TestValidatorResolvesOnlyCredentialsForRemoteClientWithSatisfiedPrerequisites(t *testing.T) {
	cfg := validValidationConfig()
	job := validValidationJob("alpha", true)
	cfg.Jobs["alpha"] = job
	env := &fakeEnvironment{errors: map[string]error{
		"secret:/secrets/alpha-mysql:alpha-user": errors.New("mysql secret denied"),
	}}
	reads := make([]string, 0)
	resolver := recordingResolver{read: func(path string) ([]byte, error) {
		reads = append(reads, path)
		return []byte("value\n"), nil
	}}
	remote := &fakeRemoteDependencies{}
	validator := Validator{Environment: env, Resolve: resolver, Dump: remote, Stores: remote}

	validator.Check(context.Background(), cfg, nil, Options{
		Connectivity: true,
		BinaryPath:   "/bin/ezdbbackup",
		ConfigPath:   "/etc/ezdbbackup/config.yml",
	})

	wantReads := []string{"/secrets/alpha-access", "/secrets/alpha-secret", "/secrets/alpha-session"}
	if !reflect.DeepEqual(reads, wantReads) {
		t.Fatalf("secret reads = %#v, want only S3 client credentials %#v", reads, wantReads)
	}
}

func TestValidatorResolutionFailureDoesNotPreventOtherClientOrJob(t *testing.T) {
	cfg := validValidationConfig()
	cfg.Jobs["alpha"] = validValidationJob("alpha", true)
	cfg.Jobs["bravo"] = validValidationJob("bravo", true)
	remote := &fakeRemoteDependencies{errors: map[string]error{
		"resolve-dump:alpha": errors.New("password read failed"),
		"new-store:alpha":    errors.New("credential provider failed"),
	}}

	report := newValidator(&fakeEnvironment{}, remote).Check(context.Background(), cfg, nil, Options{
		Connectivity: true,
		BinaryPath:   "/bin/ezdbbackup",
		ConfigPath:   "/etc/ezdbbackup/config.yml",
	})

	wantCalls := []string{
		"resolve-dump:alpha", "resolve-storage:alpha", "new-store:alpha",
		"resolve-dump:bravo", "dump-probe:bravo", "resolve-storage:bravo", "new-store:bravo", "store-probe:bravo-bucket",
	}
	if !reflect.DeepEqual(remote.calls, wantCalls) {
		t.Fatalf("remote calls = %#v, want %#v", remote.calls, wantCalls)
	}
	if !hasFinding(report, "alpha", "mysql_connectivity", "resolve MySQL probe options") ||
		!hasFinding(report, "alpha", "s3_connectivity", "create S3 client") {
		t.Fatalf("findings = %#v, want both independent alpha failures", report.Findings)
	}
}

type fakeEnvironment struct {
	calls  []string
	errors map[string]error
	events *[]string
}

func (f *fakeEnvironment) call(value string) error {
	f.calls = append(f.calls, value)
	if f.events != nil {
		*f.events = append(*f.events, "local:"+value)
	}
	return f.errors[value]
}

func (f *fakeEnvironment) CheckUser(name string) error {
	return f.call("user:" + name)
}

func (f *fakeEnvironment) CheckExecutable(context.Context, string) error {
	return errors.New("run-as-aware executable check must be used")
}

func (f *fakeEnvironment) CheckExecutableAs(_ context.Context, path, runAs string) error {
	return f.call("executable:" + path + ":" + runAs)
}

func (f *fakeEnvironment) CheckWritableTarget(path, runAs string) error {
	return f.call("writable:" + path + ":" + runAs)
}

func (f *fakeEnvironment) CheckSecretFile(path, runAs string) error {
	return f.call("secret:" + path + ":" + runAs)
}

func (f *fakeEnvironment) CheckCronPath(path string) error {
	return f.call("cron:" + path)
}

type fakeRemoteDependencies struct {
	calls  []string
	errors map[string]error
	events *[]string
}

func (f *fakeRemoteDependencies) record(call string) {
	f.calls = append(f.calls, call)
	if f.events != nil {
		*f.events = append(*f.events, "remote:"+call)
	}
}

func (f *fakeRemoteDependencies) Dump(job config.JobConfig) (dump.Request, error) {
	name := job.MySQL.Host
	call := "resolve-dump:" + name
	f.record(call)
	return dump.Request{Host: name}, f.errors[call]
}

func (f *fakeRemoteDependencies) Storage(job config.JobConfig) (storage.Options, error) {
	name := job.MySQL.Host
	call := "resolve-storage:" + name
	f.record(call)
	return storage.Options{Region: name}, f.errors[call]
}

func (f *fakeRemoteDependencies) Run(context.Context, dump.Request, io.Writer) error {
	return errors.New("Run must not be used by validation")
}

func (f *fakeRemoteDependencies) Probe(_ context.Context, request dump.Request) error {
	call := "dump-probe:" + request.Host
	f.record(call)
	return f.errors[call]
}

func (f *fakeRemoteDependencies) New(_ context.Context, options storage.Options) (storage.Store, error) {
	call := "new-store:" + options.Region
	f.record(call)
	if err := f.errors[call]; err != nil {
		return nil, err
	}
	return fakeStore{owner: f}, nil
}

type fakeStore struct {
	owner *fakeRemoteDependencies
}

func (fakeStore) UploadFile(context.Context, string, string, string) (storage.UploadResult, error) {
	return storage.UploadResult{}, errors.New("UploadFile must not be used by validation")
}

func (s fakeStore) Probe(_ context.Context, bucket string) error {
	call := "store-probe:" + bucket
	s.owner.record(call)
	return s.owner.errors[call]
}

type recordingResolver struct {
	read func(string) ([]byte, error)
}

func (r recordingResolver) Dump(job config.JobConfig) (dump.Request, error) {
	password, err := job.MySQL.PasswordRef().Resolve(r.read)
	return dump.Request{Host: job.MySQL.Host, Password: password}, err
}

func (r recordingResolver) Storage(job config.JobConfig) (storage.Options, error) {
	access, err := job.S3.AccessKeyIDRef().Resolve(r.read)
	if err != nil {
		return storage.Options{}, err
	}
	secret, err := job.S3.SecretAccessKeyRef().Resolve(r.read)
	if err != nil {
		return storage.Options{}, err
	}
	session, err := job.S3.SessionTokenRef().Resolve(r.read)
	return storage.Options{Region: job.MySQL.Host, Credentials: storage.Credentials{
		AccessKeyID: access, SecretAccessKey: secret, SessionToken: session, Explicit: true,
	}}, err
}

func newValidator(env Environment, remote *fakeRemoteDependencies) Validator {
	return Validator{Environment: env, Resolve: remote, Dump: remote, Stores: remote}
}

func validValidationConfig() *config.Config {
	return &config.Config{
		Version:  1,
		Defaults: config.Defaults{DumpBinary: "/dump/default", TempDir: "/tmp/default"},
		Logging: config.LoggingConfig{
			Directory: "/logs",
			Rotation:  config.RotationConfig{MaxSizeMB: 1, MaxFiles: 1, MaxAgeDays: 1},
		},
		Jobs: map[string]config.JobConfig{},
	}
}

func validValidationJob(name string, enabled bool) config.JobConfig {
	return config.JobConfig{
		Enabled: enabled, Schedule: "0 2 * * *", RunAs: name + "-user",
		DumpBinary: "/dump/" + name, TempDir: "/tmp/" + name,
		MySQL: config.MySQLConfig{
			Host: name, Port: 3306, User: "backup",
			PasswordFile: "/secrets/" + name + "-mysql",
			Databases:    config.DatabaseSelection{Names: []string{"app"}},
		},
		S3: config.S3Config{
			Bucket: name + "-bucket", Region: "us-east-1",
			AccessKeyIDFile:     "/secrets/" + name + "-access",
			SecretAccessKeyFile: "/secrets/" + name + "-secret",
			SessionTokenFile:    "/secrets/" + name + "-session",
		},
	}
}

func hasFinding(report Report, job, check, messagePart string) bool {
	return findFindingWithMessage(report, job, check, messagePart) != nil
}

func findFinding(report Report, job, check string) error {
	for _, finding := range report.Findings {
		if finding.Job == job && finding.Check == check {
			return finding
		}
	}
	return nil
}

func findFindingWithMessage(report Report, job, check, messagePart string) *Finding {
	for i := range report.Findings {
		finding := &report.Findings[i]
		if finding.Job == job && finding.Check == check && strings.Contains(finding.Message, messagePart) {
			return finding
		}
	}
	return nil
}
