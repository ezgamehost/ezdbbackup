# ezdbbackup Design Specification

**Status:** Approved for implementation planning
**Date:** 2026-08-09

## Overview

`ezdbbackup` is a statically compiled Go CLI for Linux that runs logical MySQL backups with a host-provided `mysqldump`, compresses each backup, and uploads it to Amazon S3 or an S3-compatible service. It supports multiple named backup jobs from one system-wide YAML configuration and manages those jobs' system cron entries.

The program is an on-demand CLI invoked manually or by cron. It is not a daemon.

## Goals

- Configure multiple named backup jobs in `/etc/ezdbbackup/config.yml`.
- Run one enabled job or all enabled jobs from the CLI.
- Invoke a configurable host-provided `mysqldump` executable.
- Stage a gzip-compressed backup locally before uploading it.
- Upload through the AWS SDK to AWS S3 or a custom S3 endpoint.
- Atomically manage `/etc/cron.d/ezdbbackup` from the CLI.
- Validate configuration, local dependencies, and optional remote connectivity before a backup.
- Write structured JSON Lines logs to local files and rotate them without relying on the system `logrotate` service.
- Distribute fully static Linux binaries through GitHub Actions.

## Non-goals for v1

- Restoring backups.
- Running a long-lived scheduler or daemon.
- Implementing the MySQL logical dump format in Go.
- Managing S3 retention or lifecycle policies.
- Encrypting backup contents before upload.
- Managing MySQL users, grants, or server configuration.
- Supporting non-Linux operating systems.

## Filesystem contract

| Purpose | Default path |
| --- | --- |
| Configuration | `/etc/ezdbbackup/config.yml` |
| Managed cron schedule | `/etc/cron.d/ezdbbackup` |
| Temporary backup staging | `/var/lib/ezdbbackup/tmp` |
| Error log | `/var/log/ezdbbackup/error.log` |
| Informational log | `/var/log/ezdbbackup/info.log` |
| Debug log | `/var/log/ezdbbackup/debug.log` |

The configuration path can be overridden with the global `--config` flag. The temporary directory and log directory can be changed in configuration. The cron schedule path is fixed in v1 so ownership is unambiguous.

## Configuration

The YAML document is versioned and rejects unknown fields. A representative configuration is:

```yaml
version: 1

defaults:
  dump_binary: /usr/bin/mysqldump
  temp_dir: /var/lib/ezdbbackup/tmp

logging:
  directory: /var/log/ezdbbackup
  debug: false
  rotation:
    max_size_mb: 100
    max_files: 7
    max_age_days: 30
    compress: true

jobs:
  production:
    enabled: true
    schedule: "0 2 * * *"
    run_as: root

    mysql:
      host: db.internal
      port: 3306
      user: backup
      password_file: /etc/ezdbbackup/secrets/mysql-production
      databases: [app, analytics]
      extra_args:
        - --single-transaction
        - --quick

    s3:
      bucket: database-backups
      prefix: production/mysql
      region: us-east-1
      endpoint: https://s3.example.com
      force_path_style: true
```

### General rules

- `version` must be `1`, and duplicate YAML mapping keys are rejected.
- Job names must be unique and safe to embed in cron entries and object keys. They match `[A-Za-z0-9][A-Za-z0-9_-]{0,63}`.
- `enabled` controls inclusion in `backup --all` and in the generated cron schedule.
- `schedule` is a standard five-field cron expression. Cron nicknames such as `@daily` are not supported in v1.
- `run_as` must name an existing local user because `/etc/cron.d` entries require a user field.
- Filesystem paths in configuration must be absolute.
- `dump_binary` and `temp_dir` can be set globally and overridden by a job.
- CLI flags can enable debug logging for one invocation without changing configuration.

### MySQL settings

- `host`, `port`, and `user` identify the source server. The default port is `3306`.
- `databases` is required. It is either the scalar `all` or a non-empty list of database names. `all` invokes `mysqldump --all-databases`; no database scope is inferred silently.
- A password may be supplied as `password` or `password_file`, but never both. A referenced password file is read at execution time and must be an absolute path.
- `ezdbbackup` does not generate MySQL option files. A configured password is placed in `MYSQL_PWD` only in the child `mysqldump` environment. It is never appended to the command line or inherited by unrelated processes.
- Backup and connectivity invocations both place `--no-defaults` first and therefore never read ordinary MySQL option files such as `~/.my.cnf`. Users may omit both password fields and rely on authentication that the client still supports with `--no-defaults`, including a user-managed MySQL login-path file where supported.
- `extra_args` is an optional ordered list passed to `mysqldump`. `ezdbbackup` owns the output destination, selected databases, connection fields, and configured password handling; validation rejects extra arguments that conflict with those owned settings.

### S3 settings

- `bucket` and `region` are required. `prefix` is optional and is normalized without leading or duplicate separators.
- `endpoint` is optional. When present, it must be a complete `http://` or `https://` URL. Standard TLS certificate verification remains enabled for HTTPS endpoints. Plain HTTP is allowed for explicitly configured development or private endpoints and produces a validation warning.
- `force_path_style` supports S3-compatible services that do not implement virtual-hosted-style addressing.
- Explicit S3 credentials use `access_key_id`, `secret_access_key`, and optional `session_token`. Each value also has an absolute `*_file` form. Literal and file-backed forms of the same value are mutually exclusive, and an explicit access key ID always requires an explicit secret access key.
- When explicit S3 credentials are omitted, the AWS SDK's standard credential chain is used, including environment credentials, shared AWS configuration, container credentials, and instance roles.
- The S3 uploader uses the AWS SDK's retry behavior. Because the compressed file is staged locally, upload retries do not repeat the database dump.

## CLI contract

```text
ezdbbackup backup <job> [--config <path>] [--debug]
ezdbbackup backup --all [--config <path>] [--debug]
ezdbbackup validate [<job> | --all] [--connectivity] [--config <path>] [--debug]
ezdbbackup cron install [--config <path>]
ezdbbackup cron show
ezdbbackup cron remove
ezdbbackup version
```

With no job argument, `validate` is equivalent to `validate --all`. Validation includes disabled jobs so configuration errors are found before those jobs are enabled. `backup --all` runs only enabled jobs. Naming a disabled job explicitly for backup is an error.

`backup --all` runs jobs sequentially in lexical job-name order. A failed job does not prevent later jobs from running. After lifecycle progress, the command prints one terminal-safe `<job>: succeeded` or `<job>: failed` line per result in execution order, followed by aggregate succeeded/failed counts. These outcome lines never include the underlying error text. The command exits nonzero if any selected job failed; a zero-job run prints only the zero-count aggregate.

Interactive invocations print concise human-readable progress and errors to the terminal. Persistent operational details are written as JSON logs.

## Validation

Validation is side-effect free. It never creates a directory, backup, cron schedule, or S3 object. For a missing temporary or logging directory, it verifies that the intended cron user could create the directory beneath the nearest existing parent.

The default `validate` command checks:

- YAML syntax, schema version, required fields, type correctness, and unknown fields.
- Job name, database selection, cron expression, and conflicting option rules.
- Every selected configured user exists, including users of disabled jobs.
- For every selected job, including a disabled job, `mysqldump` and the current ezdbbackup runtime are safe regular executables and respond successfully to `--version` under that job's `run_as` identity.
- The pinned configuration source remains readable and safe for every selected job's `run_as` identity.
- The temporary directory exists or can be created and is writable.
- The log directory exists or can be created and is compatible with all selected `run_as` identities.
- Referenced secret files exist, are regular readable files, and are not readable by users outside their owner or group.
- The S3 endpoint, bucket, prefix, region, addressing mode, and explicit credential fields are structurally valid.
- The executable and configuration paths can be represented safely in `/etc/cron.d/ezdbbackup`.

`validate --connectivity` performs all default checks and then:

- Runs the configured dump executable in a non-mutating connection probe for each selected job, using the same connection and authentication path as a backup while requesting no table data or DDL and discarding stdout.
- Makes a non-mutating S3 bucket access request through the configured endpoint and credential provider.

Connectivity validation first requires the process's complete credentials to match every selected job's `run_as`, using the same exact identity rule as backup execution. Any mismatch is an error and prevents every selected configured executable/version/connectivity probe and S3 client construction. Jobs with distinct `run_as` identities must therefore be validated in separate invocations, for example `sudo -u <run_as> ezdbbackup validate <job> --connectivity`. Connectivity results identify MySQL and S3 separately. An S3 connectivity failure may reflect a policy that permits object upload but denies bucket inspection; the diagnostic must state that limitation rather than claiming the configuration can never upload.

## Backup execution

Each job uses this lifecycle:

1. Load and validate the configuration and selected job.
2. Initialize local logging.
3. Create a unique gzip destination in the configured temporary directory with mode `0600`.
4. Start `mysqldump` with an explicit argument vector and a job-specific child environment.
5. Stream stdout through Go's gzip writer into the temporary file while capturing bounded stderr for diagnostics.
6. Close the gzip stream and file, then verify that `mysqldump` exited successfully.
7. Upload the completed file to the configured S3 destination.
8. Record the object key, compressed byte count, duration, and result in logs.
9. Remove the temporary backup on success or failure.

The dump is never uploaded when `mysqldump`, compression, or local file finalization fails. Partial multipart uploads are aborted when the S3 client reports failure. Temporary-file cleanup is attempted on every exit path, and cleanup errors are logged without masking an earlier primary error.

The S3 object key is derived from the job name and backup start time in UTC:

```text
<prefix>/<job>/<YYYY>/<MM>/<DD>/<job>-<UTC timestamp>.sql.gz
```

The prefix segment and its separator are omitted when `prefix` is empty. The timestamp includes sub-second precision to prevent a manual and scheduled run that start in the same second from selecting the same key.

## Cron management

`ezdbbackup cron install`:

1. Requires sufficient permissions to manage `/etc/cron.d`.
2. Validates the complete configuration before changing the schedule.
3. Resolves the current `ezdbbackup` executable to an absolute path.
4. Renders one entry for every enabled job in lexical job-name order using its five-field schedule and `run_as` user.
5. Writes a temporary file in `/etc/cron.d`, sets mode `0644`, and atomically renames it to `/etc/cron.d/ezdbbackup`.

The file contains a generated-file warning and a stable ownership marker. Install and remove refuse to overwrite or delete an existing file at that path when the ownership marker is absent. No unrelated crontab or `/etc/cron.d` file is read or modified.

Each generated entry calls:

```text
<absolute binary> backup <job> --config <absolute config path>
```

The managed file does not configure or depend on `logrotate`. `cron show` displays the managed schedule and reports whether its ownership marker is valid. `cron remove` removes only the marked managed file and succeeds without changes when the file is already absent.

## JSON logging and rotation

Logs use JSON Lines: one complete JSON object per line. Every event contains:

- UTC timestamp
- Severity level
- Message
- CLI command
- Job name when applicable
- Backup stage when applicable
- Structured context relevant to the event

Secrets, credential-bearing environment values, and password-like command arguments are redacted before serialization. Raw configuration objects are never logged.

Events are routed by level:

- Errors go to `error.log`.
- Informational and warning events go to `info.log`.
- Debug events go to `debug.log` only while debug mode is enabled.

Built-in rotation is size- and age-aware. By default, a file rotates when it exceeds 100 MiB, up to seven rotated files are retained, files older than 30 days are removed, and rotated files are gzip-compressed. All four settings are configurable. Rotation is checked whenever a command opens or writes a log, and inter-process file locking prevents concurrent cron invocations from racing during append or rotation.

If the log directory or required log files cannot be initialized, the command reports the failure to stderr and exits before starting `mysqldump`. Logging never depends on the host's `logrotate` installation or configuration.

## Errors and exit behavior

Errors identify the job, failed stage, and underlying cause without including secrets. At minimum, stages distinguish configuration, validation, dump startup, dump execution, compression, temporary storage, S3 upload, cleanup, logging, and cron management.

Exit codes are stable at the category level:

- `0`: every requested operation succeeded.
- `1`: one or more requested jobs failed at runtime.
- `2`: command usage or configuration validation failed.
- `3`: a system-management operation, such as cron installation, failed.

## Component boundaries

The Go implementation should keep these responsibilities independent:

- **CLI:** argument parsing, command dispatch, terminal summaries, and exit codes.
- **Configuration:** strict YAML decoding, defaulting, semantic validation, and secret references.
- **Validator:** local dependency checks and optional remote connectivity checks.
- **Dump runner:** safe `mysqldump` invocation and bounded stderr capture.
- **Stager:** temporary-file lifecycle and gzip compression.
- **Object store:** S3 client construction, object naming, upload, retry, and multipart cleanup.
- **Cron manager:** deterministic rendering and safe atomic ownership of the schedule file.
- **Logger:** JSON encoding, redaction, level routing, process-safe append, and built-in rotation.

Each component exposes a small interface so its failure modes can be tested without invoking real MySQL, S3, cron, or system paths.

## Testing strategy

Unit tests cover:

- Strict config decoding, defaulting, validation, and secret-source conflicts.
- Database selection and protection against conflicting `extra_args`.
- S3 endpoint parsing, prefix normalization, and object-key generation.
- Cron parsing, deterministic rendering, ownership markers, and refusal to replace unowned files.
- JSON schema fields, secret redaction, level routing, and rotation retention.
- Exit-code selection and multi-job result summaries.

Integration tests cover:

- Successful and failing fake `mysqldump` executables.
- Streaming gzip output to a staged file and cleaning it on every failure path.
- Uploads to an S3-compatible test service, including custom endpoints and path-style addressing.
- Upload retry and multipart-abort behavior.
- Atomic cron replacement in a temporary filesystem fixture.
- Concurrent log writers and rotation.
- Local and connectivity validation with controlled dependencies.

An end-to-end test runs a named job with a fake `mysqldump`, verifies the resulting `.sql.gz` object in the S3-compatible service, checks its decompressed contents, confirms JSON log output, and confirms local temporary cleanup.

## Build and release

GitHub Actions builds release artifacts on version tags with `CGO_ENABLED=0` for:

- `linux/amd64`
- `linux/arm64`

Builds use reproducible flags such as `-trimpath`, embed the release version for `ezdbbackup version`, and publish a SHA-256 checksum file alongside the binaries. CI verifies that each release artifact is a Linux executable with no dynamic library dependencies and runs the full test suite before publishing.

The release binary has no runtime dependency on a Go installation, gzip program, AWS CLI, cron-management helper, or `logrotate`. The target host must provide a compatible `mysqldump` or explicitly configured alternative executable and a cron daemon that reads `/etc/cron.d`.
