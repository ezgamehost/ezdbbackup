# ezdbbackup v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and release a static Linux CLI that executes named MySQL dump jobs, stages gzip artifacts, uploads them to AWS S3 or a custom S3 endpoint, manages system cron entries, validates its environment, and maintains rotated JSON logs.

**Architecture:** A thin CLI composes focused packages for strict configuration, logging, dump execution, gzip staging, S3 storage, validation, backup orchestration, and cron management. External effects sit behind small interfaces so unit tests use fakes; one tagged end-to-end suite uses a fake dump executable and LocalStack S3.

**Tech Stack:** Go 1.26, standard library, `gopkg.in/yaml.v3`, `github.com/robfig/cron/v3`, AWS SDK for Go v2, `golang.org/x/sys/unix`, Docker Compose with LocalStack for integration tests, and GitHub Actions.

## Global Constraints

- Module path: `github.com/ezgamehost/ezdbbackup`.
- Build with Go 1.26 and `CGO_ENABLED=0`.
- Release only `linux/amd64` and `linux/arm64` in v1.
- Default configuration: `/etc/ezdbbackup/config.yml`.
- Managed cron file: `/etc/cron.d/ezdbbackup`; never create a filename containing a dot.
- Default staging directory: `/var/lib/ezdbbackup/tmp`.
- Default log directory: `/var/log/ezdbbackup` with `error.log`, `info.log`, and debug-only `debug.log`.
- Do not invoke or configure `logrotate`; rotation is part of the binary.
- Do not generate MySQL option files; pass a configured password only through the child process's `MYSQL_PWD`.
- Stage one mode-`0600` gzip file before upload and remove it on every success and failure path.
- Use AWS SDK for Go v2's default credential chain when explicit S3 credentials are absent.
- Configure custom S3 endpoints with the current `BaseEndpoint` API and preserve TLS verification.
- Never serialize secrets, raw configuration, or credential-bearing process environments to logs.
- `backup --all` is sequential, lexical by job name, continues after failures, and exits `1` if any job fails.
- Stable exit categories are `0` success, `1` runtime job failure, `2` usage/configuration failure, and `3` system-management failure.
- Follow TDD for every behavior: observe a failing focused test before adding production logic.
- Push every implementation commit to the task branch immediately after committing.

## Planned File Map

| Path | Responsibility |
| --- | --- |
| `go.mod`, `go.sum` | Module identity and pinned dependencies |
| `cmd/ezdbbackup/main.go` | Process entrypoint and exit code |
| `internal/buildinfo/version.go` | Linker-injected release version |
| `internal/config/types.go` | Versioned YAML schema |
| `internal/config/database.go` | `all` or explicit database selection |
| `internal/config/load.go` | Strict one-document YAML loading and defaults |
| `internal/config/validate.go` | Pure schema and semantic findings |
| `internal/config/secret.go` | Literal/file secret references and resolution |
| `internal/logging/event.go` | JSON event and sink contract |
| `internal/logging/logger.go` | Level routing and JSON Lines append |
| `internal/logging/rotate.go` | Locked size/age/count rotation and gzip |
| `internal/logging/redact.go` | Recursive key-based secret redaction |
| `internal/dump/args.go` | Owned `mysqldump` argument construction |
| `internal/dump/runner.go` | Dump execution, bounded stderr, and connectivity probe |
| `internal/stage/gzip.go` | Mode-`0600` temporary gzip lifecycle |
| `internal/storage/storage.go` | S3-neutral store and factory interfaces |
| `internal/storage/key.go` | UTC collision-safe object keys |
| `internal/storage/s3.go` | AWS SDK v2 client, upload manager, and `HeadBucket` |
| `internal/jobresolve/options.go` | Shared config/secret mapping into dump and S3 options |
| `internal/backup/service.go` | One-job staged backup orchestration |
| `internal/backup/summary.go` | Sequential multi-job execution and results |
| `internal/validation/report.go` | Structured validation findings |
| `internal/validation/environment.go` | Local user, executable, path, and secret checks |
| `internal/validation/validator.go` | Local and optional connectivity checks |
| `internal/cron/render.go` | Deterministic system-cron rendering |
| `internal/cron/manager.go` | Marked atomic install/show/remove |
| `internal/cli/run.go` | Top-level dispatch and stable exit mapping |
| `internal/cli/backup.go` | `backup` command |
| `internal/cli/validate.go` | `validate` command |
| `internal/cli/cron.go` | `cron install/show/remove` commands |
| `internal/cli/wire.go` | Production dependency graph |
| `test/fakedump/main.go` | Controllable `mysqldump` replacement |
| `test/e2e/backup_test.go` | Tagged binary-level backup test |
| `test/compose.yml` | Pinned LocalStack S3 service |
| `config.example.yml` | Complete non-secret configuration example |
| `README.md` | Installation, configuration, CLI, cron, logs, and operations |
| `.github/workflows/ci.yml` | Unit, race, vet, build, and integration checks |
| `.github/workflows/release.yml` | Static tagged artifacts and SHA-256 checksums |

## Implementation References

- AWS custom S3 endpoints: https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html
- AWS default configuration and credential chain: https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-gosdk.html
- Strict YAML field checking: https://pkg.go.dev/gopkg.in/yaml.v3#Decoder.KnownFields

---

### Task 1: Bootstrap the module and version package

**Files:**
- Create: `go.mod`
- Create: `internal/buildinfo/version.go`
- Test: `internal/buildinfo/version_test.go`
- Create: `.gitignore`

**Interfaces:**
- Produces: `buildinfo.Version string` and `buildinfo.String() string` for the CLI and release linker flags.

- [ ] **Step 1: Initialize the module**

Run:

```bash
go mod init github.com/ezgamehost/ezdbbackup
```

Expected: `go.mod` declares `module github.com/ezgamehost/ezdbbackup` and the installed Go 1.26 language version.

- [ ] **Step 2: Write the failing version tests**

```go
package buildinfo

import "testing"

func TestStringDefaultsToDev(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = ""
	if got := String(); got != "dev" {
		t.Fatalf("String() = %q, want dev", got)
	}
}

func TestStringReturnsInjectedVersion(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "v1.2.3"
	if got := String(); got != "v1.2.3" {
		t.Fatalf("String() = %q, want v1.2.3", got)
	}
}
```

- [ ] **Step 3: Run the test and observe the expected failure**

Run: `go test ./internal/buildinfo -run TestString -v`

Expected: FAIL because `Version` and `String` are undefined.

- [ ] **Step 4: Implement the minimal version package**

```go
package buildinfo

var Version = "dev"

func String() string {
	if Version == "" {
		return "dev"
	}
	return Version
}
```

Add `/ezdbbackup` and `/dist/` to `.gitignore`.

- [ ] **Step 5: Verify and commit**

Run:

```bash
go test ./internal/buildinfo -v
go test ./...
git diff --check
git add go.mod .gitignore internal/buildinfo
git commit -m "chore: bootstrap Go module"
git push
```

Expected: all tests pass and the commit is pushed.

---

### Task 2: Implement strict configuration loading

**Files:**
- Create: `internal/config/types.go`
- Create: `internal/config/database.go`
- Create: `internal/config/load.go`
- Create: `internal/config/validate.go`
- Create: `internal/config/secret.go`
- Test: `internal/config/load_test.go`
- Test: `internal/config/validate_test.go`
- Test: `internal/config/secret_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Produces: `config.Load(path string) (*Config, Findings)` and `config.Decode(r io.Reader) (*Config, Findings)`.
- Produces: `Findings.HasErrors() bool`, `Findings.ContainsWarning(path string) bool`, and `Findings.Error() string`.
- Produces: `SecretRef.Resolve(readFile func(string) ([]byte, error)) (string, error)`.
- Produces: `Config.EnabledJobNames() []string` and `Config.JobNames() []string`, both lexically sorted.
- Produces fully defaulted `JobConfig.DumpBinary`, `JobConfig.TempDir`, and `MySQLConfig.Port` values for later packages.

- [ ] **Step 1: Add the YAML and cron parser dependencies**

Run:

```bash
go get gopkg.in/yaml.v3
go get github.com/robfig/cron/v3
```

Expected: `go.mod` and `go.sum` record both modules.

- [ ] **Step 2: Write failing decode tests**

Cover a valid two-job document, scalar `databases: all`, a database sequence, default port `3306`, default dump/temp/log paths, default rotation values `100/7/30/compress=true`, an unknown field, a duplicate YAML key, a second YAML document, and malformed YAML.

Use this core assertion:

```go
func TestDecodeValidConfigAppliesDefaults(t *testing.T) {
	input := `
version: 1
jobs:
  production:
    enabled: true
    schedule: "0 2 * * *"
    run_as: root
    mysql:
      host: db.internal
      user: backup
      databases: all
    s3:
      bucket: backups
      region: us-east-1
`
	cfg, findings := Decode(strings.NewReader(input))
	if findings.HasErrors() {
		t.Fatalf("Decode() findings = %v", findings)
	}
	job := cfg.Jobs["production"]
	if job.MySQL.Port != 3306 ||
		job.DumpBinary != "/usr/bin/mysqldump" ||
		job.TempDir != "/var/lib/ezdbbackup/tmp" ||
		!job.MySQL.Databases.All {
		t.Fatalf("defaults not applied: %#v", job)
	}
}
```

- [ ] **Step 3: Run the decode tests and observe failure**

Run: `go test ./internal/config -run 'TestDecode|TestDatabase' -v`

Expected: FAIL because the config package does not exist.

- [ ] **Step 4: Define the exact schema**

Use these public shapes:

```go
type Config struct {
	Version  int                  `yaml:"version"`
	Defaults Defaults             `yaml:"defaults"`
	Logging  LoggingConfig        `yaml:"logging"`
	Jobs     map[string]JobConfig `yaml:"jobs"`
}

type Defaults struct {
	DumpBinary string `yaml:"dump_binary"`
	TempDir   string `yaml:"temp_dir"`
}

type LoggingConfig struct {
	Directory string         `yaml:"directory"`
	Debug     bool           `yaml:"debug"`
	Rotation  RotationConfig `yaml:"rotation"`
}

type RotationConfig struct {
	MaxSizeMB  int  `yaml:"max_size_mb"`
	MaxFiles   int  `yaml:"max_files"`
	MaxAgeDays int  `yaml:"max_age_days"`
	Compress   bool `yaml:"compress"`
}

type JobConfig struct {
	Enabled    bool        `yaml:"enabled"`
	Schedule   string      `yaml:"schedule"`
	RunAs      string      `yaml:"run_as"`
	DumpBinary string      `yaml:"dump_binary"`
	TempDir    string      `yaml:"temp_dir"`
	MySQL      MySQLConfig `yaml:"mysql"`
	S3         S3Config    `yaml:"s3"`
}

type MySQLConfig struct {
	Host         string            `yaml:"host"`
	Port         int               `yaml:"port"`
	User         string            `yaml:"user"`
	Password     string            `yaml:"password"`
	PasswordFile string            `yaml:"password_file"`
	Databases    DatabaseSelection `yaml:"databases"`
	ExtraArgs    []string          `yaml:"extra_args"`
}

type S3Config struct {
	Bucket              string `yaml:"bucket"`
	Prefix              string `yaml:"prefix"`
	Region              string `yaml:"region"`
	Endpoint            string `yaml:"endpoint"`
	ForcePathStyle      bool   `yaml:"force_path_style"`
	AccessKeyID         string `yaml:"access_key_id"`
	AccessKeyIDFile     string `yaml:"access_key_id_file"`
	SecretAccessKey     string `yaml:"secret_access_key"`
	SecretAccessKeyFile string `yaml:"secret_access_key_file"`
	SessionToken        string `yaml:"session_token"`
	SessionTokenFile    string `yaml:"session_token_file"`
}
```

`DatabaseSelection.UnmarshalYAML` must accept only the exact scalar `all` or a non-empty sequence of non-empty strings. `Decode` must use `yaml.Decoder.KnownFields(true)`, reject a second document, apply defaults, normalize S3 prefixes, and return findings instead of calling `os.Exit`.

- [ ] **Step 5: Run decode tests to green**

Run: `go test ./internal/config -run 'TestDecode|TestDatabase' -v`

Expected: PASS.

- [ ] **Step 6: Write failing semantic validation tests**

Use table cases for wrong version, invalid job name, invalid five-field cron, cron nickname, missing run user, relative paths, missing database scope, both password forms, conflicting owned dump arguments, missing S3 bucket/region, malformed endpoint, HTTP warning, incomplete explicit S3 credentials, and literal/file conflicts.

```go
func TestValidateWarnsForPlainHTTPEndpoint(t *testing.T) {
	cfg := validConfig()
	job := cfg.Jobs["production"]
	job.S3.Endpoint = "http://127.0.0.1:9000"
	cfg.Jobs["production"] = job
	findings := Validate(cfg)
	if !findings.ContainsWarning("jobs.production.s3.endpoint") {
		t.Fatalf("findings = %v, want HTTP endpoint warning", findings)
	}
	if findings.HasErrors() {
		t.Fatalf("unexpected errors: %v", findings)
	}
}
```

- [ ] **Step 7: Implement semantic validation and secret resolution**

Use `cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)` for exactly five fields. Reject `extra_args` that set output, host, port, user, password, result-file, all-databases, databases, or positional database names.

Represent flattened fields through:

```go
type SecretRef struct {
	Literal string
	File    string
}

func (s SecretRef) Resolve(readFile func(string) ([]byte, error)) (string, error) {
	if s.Literal != "" && s.File != "" {
		return "", errors.New("literal and file secret sources are mutually exclusive")
	}
	if s.File == "" {
		return s.Literal, nil
	}
	b, err := readFile(s.File)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
```

Add `PasswordRef()` and three S3 credential-ref methods to the schema types. Make all returned job-name slices lexical.

- [ ] **Step 8: Verify and commit**

Run:

```bash
go test ./internal/config -v
go test ./...
go vet ./...
git diff --check
git add go.mod go.sum internal/config
git commit -m "feat: add strict YAML configuration"
git push
```

Expected: config tests and repository checks pass.

---

### Task 3: Add JSON logging with built-in rotation

**Files:**
- Create: `internal/logging/event.go`
- Create: `internal/logging/logger.go`
- Create: `internal/logging/rotate.go`
- Create: `internal/logging/redact.go`
- Test: `internal/logging/logger_test.go`
- Test: `internal/logging/rotate_test.go`
- Test: `internal/logging/redact_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Produces: `logging.Sink` with `Write(Event) error`.
- Produces: `logging.New(Options) (*FileLogger, error)`.
- Produces: `logging.Event{Time, Level, Message, Command, Job, Stage, Fields}`.
- Consumes no application packages; callers map config into `logging.Options`.

- [ ] **Step 1: Add the Linux file-locking dependency**

Run: `go get golang.org/x/sys/unix`

Expected: the dependency is pinned in `go.mod` and `go.sum`.

- [ ] **Step 2: Write failing routing and redaction tests**

```go
func TestWriteRoutesAndRedacts(t *testing.T) {
	dir := t.TempDir()
	logger, err := New(Options{
		Directory: dir,
		Debug:     true,
		Rotation: RotationOptions{
			MaxSizeBytes: 1 << 20,
			MaxFiles: 7,
			MaxAge: 30 * 24 * time.Hour,
			Compress: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = logger.Write(Event{
		Time: time.Unix(1, 0).UTC(), Level: ErrorLevel, Message: "upload failed",
		Command: "backup", Job: "production", Stage: "s3_upload",
		Fields: map[string]any{"password": "hidden", "attempt": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	line := readSingleJSONLine(t, filepath.Join(dir, "error.log"))
	if line["job"] != "production" || line["password"] != "[REDACTED]" {
		t.Fatalf("line = %#v", line)
	}
}
```

Also assert that info/warn go only to `info.log`, error goes only to `error.log`, debug is omitted when disabled, and nested keys containing `password`, `secret`, `token`, `credential`, or `mysql_pwd` are redacted case-insensitively.

- [ ] **Step 3: Run focused tests and observe failure**

Run: `go test ./internal/logging -run 'TestWrite|TestRedact' -v`

Expected: FAIL because logging types are undefined.

- [ ] **Step 4: Implement event encoding and routing**

```go
type Sink interface {
	Write(Event) error
}

type Event struct {
	Time    time.Time      `json:"timestamp"`
	Level   Level          `json:"level"`
	Message string         `json:"message"`
	Command string         `json:"command"`
	Job     string         `json:"job,omitempty"`
	Stage   string         `json:"stage,omitempty"`
	Fields  map[string]any `json:"-"`
}

type Options struct {
	Directory string
	Debug     bool
	Rotation  RotationOptions
}
```

`Write` must merge a redacted copy of `Fields` into the serialized object, append one newline, and never mutate the caller's map. `New` creates the directory when authorized, initializes required files, and returns an error before work can start if initialization fails.

- [ ] **Step 5: Write failing rotation tests**

Test a 256-byte limit, `max_files=2`, age deletion with an injected clock, gzip contents, and ten goroutines writing while forced rotation occurs. Use the stable `<log>.lock` file for `unix.Flock` so renaming a log does not invalidate synchronization.

- [ ] **Step 6: Implement locked rotation**

The write critical section must:

```go
if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
	return err
}
defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

if err := rotateIfNeeded(path, options, now()); err != nil {
	return err
}
return appendLine(path, encodedLine, 0o640)
```

Rotate from highest retained suffix downward, gzip closed rotated files when enabled, then enforce both `MaxFiles` and `MaxAge`. Never compress the active log or a lock file.

- [ ] **Step 7: Verify under the race detector and commit**

Run:

```bash
go test ./internal/logging -v
go test -race ./internal/logging
go test ./...
git diff --check
git add go.mod go.sum internal/logging
git commit -m "feat: add rotated JSON logging"
git push
```

Expected: routing, redaction, rotation, and race tests pass.

---

### Task 4: Implement dump execution and gzip staging

**Files:**
- Create: `internal/dump/args.go`
- Create: `internal/dump/runner.go`
- Test: `internal/dump/args_test.go`
- Test: `internal/dump/runner_test.go`
- Create: `internal/stage/gzip.go`
- Test: `internal/stage/gzip_test.go`

**Interfaces:**
- Produces: `dump.Runner.Run(ctx context.Context, req Request, dst io.Writer) error`.
- Produces: `dump.Runner.Probe(ctx context.Context, req Request) error`.
- Produces: `stage.Stager.Stage(ctx context.Context, dir string, write func(io.Writer) error) (Artifact, error)` and `Remove(Artifact) error`.
- `dump.Request` carries binary, host, port, user, resolved password, database selection, and validated extra arguments.

- [ ] **Step 1: Write failing argument tests**

```go
func TestArgsForSelectedDatabases(t *testing.T) {
	req := Request{
		Host: "db.internal", Port: 3307, User: "backup",
		Databases: []string{"app", "analytics"},
		ExtraArgs: []string{"--single-transaction"},
	}
	want := []string{
		"--host=db.internal", "--port=3307", "--user=backup",
		"--single-transaction", "--databases", "app", "analytics",
	}
	if got := Args(req); !slices.Equal(got, want) {
		t.Fatalf("Args() = %#v, want %#v", got, want)
	}
}
```

Also test `AllDatabases` emits `--all-databases` and probe arguments append `--no-data`, `--no-create-info`, and `--skip-triggers` without requesting an output file.

- [ ] **Step 2: Run argument tests and observe failure**

Run: `go test ./internal/dump -run 'TestArgs|TestProbeArgs' -v`

Expected: FAIL because the dump package does not exist.

- [ ] **Step 3: Implement the runner contract and child environment**

```go
type Request struct {
	Binary       string
	Host         string
	Port         int
	User         string
	Password     string
	AllDatabases bool
	Databases    []string
	ExtraArgs    []string
}

type Runner interface {
	Run(context.Context, Request, io.Writer) error
	Probe(context.Context, Request) error
}

type ExecRunner struct {
	StderrLimit int64
}
```

`ExecRunner.Run` uses `exec.CommandContext`, an explicit argument slice, stdout set to the supplied writer, and a capped stderr buffer of 64 KiB by default. Construct the child environment by removing any inherited `MYSQL_PWD` and appending one only when `Request.Password` is non-empty. Error messages include exit status and bounded stderr but never arguments or environment values.

`Probe` invokes the same binary and auth path with the no-data/no-DDL flags and `io.Discard`.

- [ ] **Step 4: Write failing runner tests**

Create executable shell fixtures under `t.TempDir()` for success, failure with stderr, version/probe capture, and context cancellation. Assert the dump output arrives at the writer, the helper sees `MYSQL_PWD`, the parent environment is unchanged, and the error never contains the password.

- [ ] **Step 5: Run runner tests to green**

Run: `go test ./internal/dump -v`

Expected: PASS.

- [ ] **Step 6: Write failing staging tests**

```go
func TestStageCreatesGzipArtifactAndRemoveDeletesIt(t *testing.T) {
	s := GzipStager{}
	artifact, err := s.Stage(context.Background(), t.TempDir(), func(w io.Writer) error {
		_, err := io.WriteString(w, "CREATE TABLE example(id INT);\n")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	assertMode(t, artifact.Path, 0o600)
	assertGzipContents(t, artifact.Path, "CREATE TABLE example(id INT);\n")
	if err := s.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact still exists: %v", err)
	}
}
```

Also assert callback failure closes and removes the partial file and that `Artifact.Size` matches the final compressed size.

- [ ] **Step 7: Implement `GzipStager`**

```go
type Artifact struct {
	Path string
	Size int64
}

type Stager interface {
	Stage(context.Context, string, func(io.Writer) error) (Artifact, error)
	Remove(Artifact) error
}
```

Create the directory when permitted, create a unique `ezdbbackup-*.sql.gz` file, force mode `0600`, run the callback through `gzip.NewWriter`, close gzip before the file, stat the final size, and remove the file after every unsuccessful stage.

- [ ] **Step 8: Verify and commit**

Run:

```bash
go test ./internal/dump ./internal/stage -v
go test ./...
go vet ./...
git diff --check
git add internal/dump internal/stage
git commit -m "feat: add dump and staging pipeline"
git push
```

Expected: argument, process, cancellation, gzip, mode, and cleanup tests pass.

---

### Task 5: Implement S3 storage and object naming

**Files:**
- Create: `internal/storage/storage.go`
- Create: `internal/storage/key.go`
- Create: `internal/storage/s3.go`
- Test: `internal/storage/key_test.go`
- Test: `internal/storage/s3_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Produces: `storage.Factory.New(ctx context.Context, Options) (Store, error)`.
- Produces: `Store.UploadFile(ctx, bucket, key, path) (UploadResult, error)` and `Store.Probe(ctx, bucket) error`.
- Produces: `storage.ObjectKey(prefix, job string, started time.Time) string`.
- Consumes resolved credential strings; this package never reads config files.

- [ ] **Step 1: Add AWS SDK v2 modules**

Run:

```bash
go get github.com/aws/aws-sdk-go-v2/config
go get github.com/aws/aws-sdk-go-v2/credentials
go get github.com/aws/aws-sdk-go-v2/feature/s3/manager
go get github.com/aws/aws-sdk-go-v2/service/s3
```

- [ ] **Step 2: Write failing object-key tests**

```go
func TestObjectKey(t *testing.T) {
	started := time.Date(2026, 8, 9, 2, 3, 4, 123456789, time.FixedZone("offset", 3600))
	got := ObjectKey("/production//mysql/", "primary", started)
	want := "production/mysql/primary/2026/08/09/primary-20260809T010304.123456789Z.sql.gz"
	if got != want {
		t.Fatalf("ObjectKey() = %q, want %q", got, want)
	}
}
```

Also assert an empty prefix begins with `<job>/` and two distinct nanoseconds produce different keys.

- [ ] **Step 3: Implement storage interfaces and key generation**

```go
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Explicit        bool
}

type Options struct {
	Region         string
	Endpoint       string
	ForcePathStyle bool
	Credentials    Credentials
}

type UploadResult struct {
	Location string
	ETag     string
}

type Store interface {
	UploadFile(context.Context, string, string, string) (UploadResult, error)
	Probe(context.Context, string) error
}

type Factory interface {
	New(context.Context, Options) (Store, error)
}
```

`ObjectKey` normalizes prefix separators and formats UTC using `20060102T150405.000000000Z`.

- [ ] **Step 4: Write failing S3 adapter tests**

Define small internal interfaces matching AWS `HeadBucket` and manager `Upload` methods. Fake them to assert bucket, key, file body, and errors. Test that a failed upload returns the SDK error and that `Probe` invokes `HeadBucket` exactly once.

- [ ] **Step 5: Implement AWS SDK v2 construction and upload**

Load SDK configuration with an explicit region. If `Credentials.Explicit` is true, use `credentials.NewStaticCredentialsProvider`; otherwise add no credentials override so the SDK default chain remains intact.

```go
client := s3.NewFromConfig(cfg, func(o *s3.Options) {
	if opts.Endpoint != "" {
		o.BaseEndpoint = aws.String(opts.Endpoint)
	}
	o.UsePathStyle = opts.ForcePathStyle
})
uploader := manager.NewUploader(client, func(u *manager.Uploader) {
	u.LeavePartsOnError = false
})
```

`UploadFile` opens and closes the staged file, passes it to `manager.Uploader`, and returns location and ETag without exposing credentials. `Probe` uses `HeadBucket`.

- [ ] **Step 6: Verify and commit**

Run:

```bash
go test ./internal/storage -v
go test ./...
go vet ./...
git diff --check
git add go.mod go.sum internal/storage
git commit -m "feat: add S3-compatible storage"
git push
```

Expected: key and adapter tests pass with no network access.

---

### Task 6: Build the backup orchestrator

**Files:**
- Create: `internal/jobresolve/options.go`
- Test: `internal/jobresolve/options_test.go`
- Create: `internal/backup/service.go`
- Create: `internal/backup/summary.go`
- Test: `internal/backup/service_test.go`
- Test: `internal/backup/summary_test.go`

**Interfaces:**
- Produces: `jobresolve.OptionsResolver`, implemented by `jobresolve.Resolver`, with `Dump(config.JobConfig) (dump.Request, error)` and `Storage(config.JobConfig) (storage.Options, error)`.
- Consumes: `config.JobConfig`, `dump.Runner`, `stage.Stager`, `storage.Factory`, and `logging.Sink`.
- Produces: `Service.Run(ctx, jobName, job) (Result, error)`.
- Produces: `Service.RunMany(ctx, cfg, names) Summary` with lexical sequential execution.

- [ ] **Step 1: Write failing resolver and one-job orchestration tests**

Use a fake dump runner that writes known SQL, the real `GzipStager`, a fake store that opens and gunzips the staged file, and a memory log sink.

Assert:

- the resolver maps every connection, database, endpoint, prefix, and path-style field;
- password and S3 secret files are resolved at execution time;
- object key uses the injected start time;
- uploaded content equals the fake SQL;
- the temporary file exists during upload and is gone afterward;
- dump failure prevents store creation and upload;
- upload failure still removes the artifact;
- cleanup error is returned only when there is no earlier primary error;
- logged events contain job, stage, size, duration, and object key without secret values.

- [ ] **Step 2: Run the focused test and observe failure**

Run: `go test ./internal/backup -run TestRun -v`

Expected: FAIL because the backup package does not exist.

- [ ] **Step 3: Define service dependencies and result types**

```go
type Resolver struct {
	ReadFile func(string) ([]byte, error)
}

type OptionsResolver interface {
	Dump(config.JobConfig) (dump.Request, error)
	Storage(config.JobConfig) (storage.Options, error)
}

type Service struct {
	Resolve jobresolve.OptionsResolver
	Dump    dump.Runner
	Stager  stage.Stager
	Stores  storage.Factory
	Log     logging.Sink
	Now     func() time.Time
}

type Result struct {
	Job       string
	ObjectKey string
	Size      int64
	Duration  time.Duration
}
```

Implement `Resolver` in `internal/jobresolve` and test it separately. It maps a defaulted job to `dump.Request` and `storage.Options`, resolving secret references through the injected reader. Explicit S3 credentials are enabled only when either access-key source is set; all three credential fields resolve before constructing the store. Both backup and validation use this resolver so password and credential behavior cannot drift.

- [ ] **Step 4: Implement the staged lifecycle**

Define `Run` with named `result Result` and `err error` returns. Use one deferred cleanup immediately after `Stage` succeeds:

```go
artifact, err := s.Stager.Stage(ctx, job.TempDir, func(w io.Writer) error {
	return s.Dump.Run(ctx, dumpRequest, w)
})
if err != nil {
	return Result{}, stageError("dump_execution", err)
}
defer func() {
	if removeErr := s.Stager.Remove(artifact); removeErr != nil {
		if err == nil {
			err = stageError("cleanup", removeErr)
		} else {
			_ = s.Log.Write(logging.Event{
				Time: s.Now(), Level: logging.ErrorLevel,
				Message: "temporary backup cleanup failed",
				Command: "backup", Job: jobName, Stage: "cleanup",
				Fields: map[string]any{"error": removeErr.Error()},
			})
		}
	}
}()
```

Structure the function with named return values so a cleanup error becomes the result only when the dump and upload succeeded. Log each stage transition and final result through structured fields.

- [ ] **Step 5: Run one-job tests to green**

Run: `go test ./internal/backup -run TestRun -v`

Expected: PASS.

- [ ] **Step 6: Write failing multi-job summary tests**

```go
func TestRunManyIsLexicalAndContinuesAfterFailure(t *testing.T) {
	cfg := configWithJobs("zeta", "alpha", "middle")
	service := serviceRecordingOrder([]string{"middle"})
	summary := service.RunMany(context.Background(), cfg, nil)
	if got := summary.JobNames(); !slices.Equal(got, []string{"alpha", "middle", "zeta"}) {
		t.Fatalf("order = %v", got)
	}
	if !summary.HasFailures() || len(summary.Results) != 3 {
		t.Fatalf("summary = %#v", summary)
	}
}
```

An empty name slice means all enabled jobs. Explicit names are sorted before execution, and disabled or missing jobs are rejected before any dump starts.

- [ ] **Step 7: Implement summary execution and commit**

Run:

```bash
go test ./internal/backup -v
go test ./...
go vet ./...
git diff --check
git add internal/jobresolve internal/backup
git commit -m "feat: orchestrate backup jobs"
git push
```

Expected: all backup lifecycle and summary tests pass.

---

### Task 7: Implement local and connectivity validation

**Files:**
- Create: `internal/validation/report.go`
- Create: `internal/validation/environment.go`
- Create: `internal/validation/validator.go`
- Test: `internal/validation/environment_test.go`
- Test: `internal/validation/validator_test.go`

**Interfaces:**
- Consumes: fully decoded `config.Config`, `jobresolve.OptionsResolver`, `dump.Runner`, and `storage.Factory`.
- Produces: `validation.Checker.Check(ctx, cfg, jobNames, Options) Report`.
- Produces findings with severity, job, check name, message, and wrapped cause.

- [ ] **Step 1: Write failing report and local-environment tests**

Test:

- report aggregation and `HasErrors`;
- existing executable is regular, absolute, executable by the intended `run_as` user, and passes `--version`;
- missing executable and non-executable file;
- existing writable directory;
- missing directory whose nearest parent is not writable by the configured cron user;
- secret file is absolute, regular, readable by the cron user, and has no other-user permission bits;
- configured cron user exists;
- binary and config paths are absolute and contain no newline or NUL.

- [ ] **Step 2: Define testable environment boundaries**

```go
type Environment interface {
	CheckUser(name string) error
	CheckExecutable(ctx context.Context, path string) error
	CheckWritableTarget(path, runAs string) error
	CheckSecretFile(path, runAs string) error
	CheckCronPath(path string) error
}

type Options struct {
	Connectivity bool
	BinaryPath   string
	ConfigPath   string
}

type Checker interface {
	Check(context.Context, *config.Config, []string, Options) Report
}
```

`OSEnvironment` uses `os/user`, file ownership/mode metadata, and `exec.CommandContext(path, "--version")`. It inspects the nearest existing parent for missing directories but never creates one.

- [ ] **Step 3: Run local tests to green**

Run: `go test ./internal/validation -run 'TestReport|TestOSEnvironment' -v`

Expected: PASS.

- [ ] **Step 4: Write failing validator tests**

Use fake environment, dump runner, and storage factory to prove:

- every configured job, including disabled jobs, is checked when names are empty;
- a selected job limits validation;
- local findings are returned without remote calls by default;
- `Connectivity=true` invokes dump `Probe` and S3 `HeadBucket` separately;
- an S3 probe failure message says bucket inspection may be denied even when uploads are allowed;
- one job's failures do not prevent checks for remaining jobs;
- explicit credential files are resolved only for the corresponding S3 client.

- [ ] **Step 5: Implement the validation coordinator**

```go
type Validator struct {
	Environment Environment
	Resolve     jobresolve.OptionsResolver
	Dump        dump.Runner
	Stores      storage.Factory
}
```

Run pure config validation first, then local checks for selected jobs in lexical order. Skip a dependent remote check when its local prerequisites failed, and add a precise skipped finding. Connectivity probes are read-only and run sequentially.

- [ ] **Step 6: Verify and commit**

Run:

```bash
go test ./internal/validation -v
go test -race ./internal/validation
go test ./...
go vet ./...
git diff --check
git add internal/validation
git commit -m "feat: validate backup environments"
git push
```

Expected: local and remote coordination tests pass.

---

### Task 8: Implement safe cron management

**Files:**
- Create: `internal/cron/render.go`
- Create: `internal/cron/manager.go`
- Test: `internal/cron/render_test.go`
- Test: `internal/cron/manager_test.go`

**Interfaces:**
- Consumes: `config.Config` and absolute executable/config paths.
- Produces: `Render(cfg, binaryPath, configPath) ([]byte, error)`.
- Produces: `Manager.Install(content []byte) error`, `Show() ([]byte, error)`, and `Remove() error`.
- Default manager path is exactly `/etc/cron.d/ezdbbackup` and is injectable in tests.

- [ ] **Step 1: Write failing render tests**

Assert a fixed header, ownership marker `# ezdbbackup-managed: v1`, `SHELL=/bin/sh`, a stable `PATH`, lexical enabled jobs only, user field placement, POSIX shell quoting, terminal newline, rejection of invalid schedules, and no `logrotate` content.

Expected line:

```text
0 2 * * * root '/usr/local/bin/ezdbbackup' backup 'production' --config '/etc/ezdbbackup/config.yml'
```

- [ ] **Step 2: Run render tests and observe failure**

Run: `go test ./internal/cron -run TestRender -v`

Expected: FAIL because cron rendering is undefined.

- [ ] **Step 3: Implement deterministic rendering**

Parse schedules with the same five-field cron parser used by config. Use a private POSIX single-quote function that renders an embedded quote as `'"'"'`. Render only enabled jobs, sorted lexically, and reject non-absolute binary/config paths or values containing newline or NUL.

- [ ] **Step 4: Write failing manager tests**

Under `t.TempDir()`, assert:

- install writes mode `0644` and exact content;
- replacing a marked file is atomic;
- an unmarked existing file is never overwritten;
- show rejects an unmarked file;
- remove deletes a marked file;
- remove rejects an unmarked file;
- remove succeeds when absent;
- a simulated rename failure leaves the prior managed file intact.

- [ ] **Step 5: Implement atomic ownership-aware management**

```go
const (
	DefaultPath     = "/etc/cron.d/ezdbbackup"
	OwnershipMarker = "# ezdbbackup-managed: v1"
)

type Manager struct {
	Path string
}
```

Create the temporary file in the destination directory, write and sync it, chmod `0644`, close it, then rename. Before replace/show/remove, read any existing file and require the exact marker in its header. Clean up only the temporary file created by the current failed install.

- [ ] **Step 6: Verify and commit**

Run:

```bash
go test ./internal/cron -v
go test ./...
go vet ./...
git diff --check
git add internal/cron
git commit -m "feat: manage system cron schedule"
git push
```

Expected: rendering, ownership, atomicity, and refusal tests pass.

---

### Task 9: Wire the CLI and stable exit behavior

**Files:**
- Create: `internal/cli/run.go`
- Create: `internal/cli/backup.go`
- Create: `internal/cli/validate.go`
- Create: `internal/cli/cron.go`
- Create: `internal/cli/wire.go`
- Test: `internal/cli/run_test.go`
- Test: `internal/cli/backup_test.go`
- Test: `internal/cli/validate_test.go`
- Test: `internal/cli/cron_test.go`
- Create: `cmd/ezdbbackup/main.go`

**Interfaces:**
- Consumes every component through injected CLI interfaces.
- Produces: `cli.Run(ctx context.Context, args []string, deps Dependencies) int`.
- Produces: `cli.DefaultDependencies() Dependencies` for the real binary.
- Main performs only dependency construction and `os.Exit`.

- [ ] **Step 1: Write failing dispatch and usage tests**

Table-test empty args, unknown command, `version`, `backup`, `validate`, and `cron install/show/remove`. Assert exit `2` for malformed flags and that stdout/stderr contain concise human-readable text rather than JSON.

```go
func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := fakeDependencies(&stdout, &stderr)
	deps.Version = "v1.2.3"
	if code := Run(context.Background(), []string{"version"}, deps); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if got := stdout.String(); got != "ezdbbackup v1.2.3\n" {
		t.Fatalf("stdout = %q", got)
	}
}
```

- [ ] **Step 2: Define dependency interfaces**

```go
type Dependencies struct {
	Stdout         io.Writer
	Stderr         io.Writer
	Version        string
	LoadConfig     func(string) (*config.Config, config.Findings)
	NewLogger      func(logging.Options) (logging.Sink, error)
	NewBackup      func(logging.Sink) *backup.Service
	Validator      validation.Checker
	Cron           CronService
	ExecutablePath func() (string, error)
}

type CronService interface {
	Install([]byte) error
	Show() ([]byte, error)
	Remove() error
}
```

Keep parsing in the standard library with one `flag.FlagSet` per command and `ContinueOnError` so tests own output and no package calls `os.Exit`.

- [ ] **Step 3: Implement dispatch and version**

Run: `go test ./internal/cli -run 'TestVersion|TestUsage|TestUnknown' -v`

Expected: PASS.

- [ ] **Step 4: Write failing backup command tests**

Assert:

- default config path and `--config` override;
- exactly one of job or `--all` is required;
- disabled/missing explicit job returns `2` before logging or dumping;
- logging initialization failure returns `1` before dump;
- `--debug` enables the debug file option;
- all-job output is lexical and includes every result;
- any runtime job failure returns `1` after remaining jobs complete.

- [ ] **Step 5: Implement backup command**

Load and validate the YAML, run side-effect-free local environment validation for selected jobs, initialize logging from config, create the backup service with that sink, then call `RunMany`. Print one line per result and a final count. The `validate` command never initializes file logging, so it cannot create the log directory.

- [ ] **Step 6: Write failing validate and cron command tests**

Validate tests cover default all jobs, explicit job, `--all` conflict, `--connectivity` propagation, warnings with exit `0`, and errors with exit `2`.

Cron tests cover:

- install runs full validation before rendering or writing;
- validation failure leaves the file unchanged and exits `2`;
- permission or atomic-write failure exits `3`;
- show prints exactly the managed file;
- remove is idempotent;
- cron commands do not create `logrotate` configuration.

- [ ] **Step 7: Implement validate and cron commands**

`cron install` resolves `os.Executable`, calls `cron.Render` using the effective absolute config path, then invokes the manager only after the report has no errors. `cron show` and `cron remove` operate on the fixed path without requiring a valid config.

- [ ] **Step 8: Add production wiring and main**

```go
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(cli.Run(ctx, os.Args[1:], cli.DefaultDependencies()))
}
```

`DefaultDependencies` constructs one `jobresolve.Resolver` shared by backup and validation, plus `dump.ExecRunner`, `stage.GzipStager`, `storage.AWSFactory`, `validation.Validator`, and `cron.Manager{Path: cron.DefaultPath}`. Map configured log megabytes/days into byte/duration options in one pure helper with unit tests.

- [ ] **Step 9: Verify CLI behavior and commit**

Run:

```bash
go test ./internal/cli -v
go test ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o ezdbbackup ./cmd/ezdbbackup
./ezdbbackup version
./ezdbbackup 2>&1
test $? -eq 2
git diff --check
git add cmd internal/cli
git commit -m "feat: expose ezdbbackup CLI"
git push
```

Expected: tests pass, the static-compatible binary builds, version prints `dev`, and no-argument usage exits `2`.

---

### Task 10: Add binary-level S3 integration coverage

**Files:**
- Create: `test/fakedump/main.go`
- Create: `test/e2e/backup_test.go`
- Create: `test/compose.yml`
- Create: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes only the public CLI contract and real filesystem/S3 protocols.
- Produces an integration test selected with the `integration` build tag.

- [ ] **Step 1: Create the controllable fake dump executable**

```go
package main

import (
	"fmt"
	"os"
)

func main() {
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			fmt.Println("mysqldump fake 1.0")
			return
		}
	}
	if os.Getenv("FAKE_DUMP_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "forced dump failure")
		os.Exit(9)
	}
	fmt.Println("CREATE TABLE example(id INT);")
	fmt.Println("INSERT INTO example VALUES (1);")
}
```

- [ ] **Step 2: Add a pinned LocalStack service**

`test/compose.yml` uses `localstack/localstack:4.14.0`, exposes `4566`, limits services to S3, and defines a health check against `/_localstack/health`. Use test credentials `test/test` only.

- [ ] **Step 3: Write the failing tagged end-to-end test**

The test must:

1. Build `./cmd/ezdbbackup` and `./test/fakedump` into `t.TempDir()`.
2. Create an S3 bucket through AWS SDK v2 at `EZDBBACKUP_TEST_S3_ENDPOINT`.
3. Write a complete config using temporary stage/log paths and `force_path_style: true`.
4. Execute `ezdbbackup validate --all --connectivity --config <path>`.
5. Execute `ezdbbackup backup production --config <path>`.
6. List and fetch the single object.
7. Gunzip and compare exact SQL.
8. Parse each JSON log line and assert job/stage fields.
9. Assert the staging directory contains no `.sql.gz` files.

Run:

```bash
docker compose -f test/compose.yml up -d --wait
trap 'docker compose -f test/compose.yml down -v' EXIT
AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1 EZDBBACKUP_TEST_S3_ENDPOINT=http://127.0.0.1:4566 go test -tags=integration ./test/e2e -v
```

Expected before finishing the wiring: FAIL at the first behavior not yet compatible with a real S3 endpoint.

- [ ] **Step 4: Fix only integration defects and rerun**

Keep fixes inside the owning packages. Do not add LocalStack-specific production behavior. The custom endpoint must continue to use `BaseEndpoint` and `UsePathStyle`.

Run the command block from Step 3 again.

Expected: PASS and the container is removed.

- [ ] **Step 5: Add integration execution to CI and commit**

Create `.github/workflows/ci.yml` with jobs for:

- `actions/checkout@v4` and `actions/setup-go@v5` using `go-version-file: go.mod`;
- `go test ./...` and `go vet ./...`;
- `go test -race ./...`;
- `CGO_ENABLED=0 go build ./cmd/ezdbbackup`;
- LocalStack startup plus the exact tagged test command.

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
git diff --check
git add test .github/workflows/ci.yml
git commit -m "test: add S3 backup integration"
git push
```

Expected: local checks pass and the pushed workflow starts.

---

### Task 11: Add operations documentation and static releases

**Files:**
- Create: `config.example.yml`
- Create: `README.md`
- Create: `.github/workflows/release.yml`
- Modify: `internal/cli/run_test.go`

**Interfaces:**
- Documents the final user-facing CLI and config contract.
- Produces tag-triggered `ezdbbackup_<version>_linux_<arch>.tar.gz` artifacts and `SHA256SUMS`.

- [ ] **Step 1: Write the complete example configuration**

Include two named jobs, one using `password_file` and the AWS default credential chain, and one using explicit S3-compatible endpoint credentials through `*_file`. Include every logging rotation setting and comments explaining `databases: all` versus a list. Use clearly fictional absolute secret-file paths, never credential values.

- [ ] **Step 2: Write the README**

Document:

- supported Linux architectures and the host `mysqldump`/cron prerequisites;
- installation of the binary and directory ownership;
- all CLI commands and exit codes;
- strict configuration schema and secret-source rules;
- `MYSQL_PWD` child-process behavior and its trade-off;
- custom endpoint/path-style examples;
- `validate` versus `validate --connectivity`;
- cron file ownership and the no-dot filename requirement;
- JSON fields, file routing, debug mode, built-in rotation, and the absence of `logrotate`;
- staged-file disk sizing and cleanup;
- S3 object key layout;
- build, unit, race, and integration commands.

- [ ] **Step 3: Add a release workflow**

On tags matching `v*`:

1. Run `go test ./...` and `go vet ./...`.
2. Build `linux/amd64` and `linux/arm64` with `CGO_ENABLED=0`, `-trimpath`, and `-ldflags="-s -w -X github.com/ezgamehost/ezdbbackup/internal/buildinfo.Version=$GITHUB_REF_NAME"`.
3. Run `file` on each binary.
4. Fail if `ldd` reports any dynamic dependency.
5. Package each binary with README and example config.
6. Generate one `SHA256SUMS`.
7. Publish artifacts with `gh release create "$GITHUB_REF_NAME" dist/* --verify-tag --generate-notes` using the workflow token.

- [ ] **Step 4: Add a release-version regression test**

Build a temporary binary with:

```bash
go build -ldflags="-X github.com/ezgamehost/ezdbbackup/internal/buildinfo.Version=v9.9.9-test" -o /tmp/ezdbbackup-version-test ./cmd/ezdbbackup
/tmp/ezdbbackup-version-test version
```

Expected output: `ezdbbackup v9.9.9-test`.

Encode the same assertion in a Go test that builds to `t.TempDir()` rather than relying on `/tmp`.

- [ ] **Step 5: Verify docs, workflows, and static binaries**

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/ezdbbackup_linux_amd64 ./cmd/ezdbbackup
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/ezdbbackup_linux_arm64 ./cmd/ezdbbackup
file dist/ezdbbackup_linux_amd64 dist/ezdbbackup_linux_arm64
ldd dist/ezdbbackup_linux_amd64 2>&1 | grep -E 'not a dynamic executable|statically linked'
git diff --check
```

Expected: tests and vet pass, both files are Linux executables, and the amd64 binary reports no dynamic dependencies.

- [ ] **Step 6: Commit and push**

```bash
git add README.md config.example.yml .github/workflows/release.yml internal/cli/run_test.go
git commit -m "docs: add release and operations guide"
git push
```

Expected: documentation and release workflow are pushed.

---

### Task 12: Run the final acceptance matrix

**Files:**
- Modify only files required to correct a failing acceptance check.

**Interfaces:**
- Verifies the full specification without introducing new public behavior.

- [ ] **Step 1: Run formatting and static analysis**

Run:

```bash
test -z "$(gofmt -l $(rg --files -g '*.go'))"
go vet ./...
git diff --check
```

Expected: no output and exit `0`.

- [ ] **Step 2: Run unit and race suites without cache**

Run:

```bash
go test -count=1 ./...
go test -count=1 -race ./...
```

Expected: every package passes.

- [ ] **Step 3: Run end-to-end validation and backup**

Run:

```bash
docker compose -f test/compose.yml up -d --wait
trap 'docker compose -f test/compose.yml down -v' EXIT
AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1 EZDBBACKUP_TEST_S3_ENDPOINT=http://127.0.0.1:4566 go test -count=1 -tags=integration ./test/e2e -v
```

Expected: the test validates connectivity, uploads and verifies one gzip object, parses JSON logs, and confirms local cleanup.

- [ ] **Step 4: Verify both static release architectures**

Run:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/ezdbbackup_linux_amd64 ./cmd/ezdbbackup
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/ezdbbackup_linux_arm64 ./cmd/ezdbbackup
file dist/ezdbbackup_linux_amd64 dist/ezdbbackup_linux_arm64
ldd dist/ezdbbackup_linux_amd64 2>&1 | grep -E 'not a dynamic executable|statically linked'
```

Expected: both targets build and amd64 has no dynamic dependencies.

- [ ] **Step 5: Review the implementation against the design**

Check each heading in `docs/superpowers/specs/2026-08-09-ezdbbackup-design.md` against tests and code. Specifically verify no restore, daemon, S3 retention, pre-upload encryption, non-Linux release, generated MySQL option file, or `logrotate` integration was added.

- [ ] **Step 6: Commit any acceptance fixes and push**

If Steps 1-5 required tracked corrections, stage only modifications to already tracked files:

```bash
git add --update
git commit -m "fix: satisfy v1 acceptance checks"
git push
```

If a correction requires a new file, stop and add its exact path explicitly before committing. If no tracked correction was required, do not create an empty commit. Finish by running `git status -sb` and require a clean branch synchronized with its upstream.
