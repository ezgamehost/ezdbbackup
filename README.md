# ezdbbackup

`ezdbbackup` is an on-demand Linux CLI that runs a host-provided
`mysqldump`, writes one private gzip staging file, and uploads it to Amazon S3
or an S3-compatible service. It can run one named job, run all enabled jobs,
validate local and remote prerequisites, and safely manage one system cron
file. It is not a daemon and does not restore backups or enforce S3 retention.

## Platform and prerequisites

Release archives are provided for Linux `amd64` and `arm64`. The binary is
static and does not require Go, a `gzip` executable, the AWS CLI, a cron helper,
or `logrotate` at runtime. The host must provide:

- a compatible `mysqldump` (or another explicitly configured executable with
  compatible arguments);
- enough local disk for one complete compressed dump; and
- a cron daemon that reads `/etc/cron.d` if scheduled backups are required.

MySQL, S3, IAM, lifecycle policies, and the cron daemon itself remain operator
responsibilities.

## Install

Download the archive for the host architecture and verify it against the
release's `SHA256SUMS`. Each archive contains `ezdbbackup`, this README, and
`config.example.yml`.

The following layout uses one dedicated account for both example jobs. Adapt
the user and group to the configured `run_as` values:

```sh
sudo install -o root -g root -m 0755 ezdbbackup /usr/local/bin/ezdbbackup
sudo useradd --system --home-dir /var/lib/ezdbbackup --shell /usr/sbin/nologin ezdbbackup

sudo install -d -o root -g ezdbbackup -m 0750 /etc/ezdbbackup
sudo install -d -o root -g ezdbbackup -m 0750 /etc/ezdbbackup/secrets
sudo install -d -o ezdbbackup -g ezdbbackup -m 0700 /var/lib/ezdbbackup/tmp
sudo install -d -o ezdbbackup -g ezdbbackup -m 0750 /var/log/ezdbbackup

sudo install -o root -g ezdbbackup -m 0640 config.example.yml /etc/ezdbbackup/config.yml
sudo install -o root -g ezdbbackup -m 0640 /path/to/mysql-password /etc/ezdbbackup/secrets/production-mysql-password
```

Create the service account with the distribution's preferred account-management
tool if `useradd` is unavailable. Secret files must be regular files readable
by `run_as` and must have no permissions for “other”; mode `0640` with the
service group is a typical choice. Give `run_as` traversal permission on all
parent directories. Do not put a real secret on a command line or in shell
history while provisioning it.

The binary creates a missing staging directory with mode `0700`, a missing log
directory with mode `0750`, and log files with mode `0640`, but pre-creating
them makes ownership explicit. A process cannot change an existing directory's
ownership, so cron's `run_as` user must already be able to write both targets.

## Configure

The default configuration is `/etc/ezdbbackup/config.yml`; `--config` selects a
different file. Start from [`config.example.yml`](config.example.yml), which
contains two jobs:

- `production` selects a database list, reads its MySQL password from a file,
  and leaves S3 credentials to the AWS SDK default chain.
- `archive-compatible` selects the exact scalar `databases: all`, uses a custom
  endpoint with path-style addressing, and reads all explicit S3 credentials
  from files.

Defaults are `/usr/bin/mysqldump`, `/var/lib/ezdbbackup/tmp`,
`/var/log/ezdbbackup`, MySQL port `3306`, and rotation of 100 MiB with seven
rotated files retained for 30 days and gzip compression enabled. A job can
override `dump_binary` and `temp_dir`.

### Strict schema and job rules

The configuration loader accepts exactly one YAML document. It rejects unknown
fields, duplicate mapping keys, nulls, wrong scalar tags (including quoted
numbers for integer fields), YAML anchors, aliases and merge keys, malformed
collection types, and any `version` other than `1`. Job names must match
`[A-Za-z0-9][A-Za-z0-9_-]{0,63}`.
Schedules use the five `/etc/cron.d` time fields and support numeric/name
lists, ranges, and steps; day-of-week `0` and `7` both mean Sunday. Nicknames
such as `@daily`, Quartz `?`, and out-of-range values are rejected. Every job
requires an existing `run_as` user, MySQL host/user/database scope, and S3
bucket/region.

`databases` is required and has only two forms:

```yaml
# Every database; maps to mysqldump --all-databases.
databases: all

# Only these databases; the list must be non-empty.
databases:
  - application
  - analytics
```

Database names must not begin with `-` or contain control characters. A `--`
option terminator is inserted before selected database operands.

`extra_args` retain their listed order and must use one complete long option
per YAML item (`--name` or `--name=value`). Positional arguments, `--`, and all
short options are rejected. Names are checked case-insensitively with `_`
treated as `-`. Options are rejected when they can override host/port/user,
credentials (including multifactor password options), database/table scope or
wildcards, output/result/tab/directory/debug files, defaults or login paths;
exit early via help/version/print-defaults; or mutate the server through log
deletion, flushing, replica control, or an init command. Unsafe abbreviated
spellings are rejected too. This prevents a backup configuration from uploading
help text or writing an unprotected side copy. Normal dump-shaping long options
such as `--single-transaction` and `--quick` remain supported.

Connectivity probes do not reuse dump-shaping `extra_args`. They copy only an
audited set of transport, authentication-plugin, and TLS options, force
no-data/no-DDL output, disable ordinary MySQL option files, table locks, and log
flushing/deletion before selecting the configured databases. Disabling option
files prevents inherited result-file, init-command, or replica controls from
making a probe write files or change server state.

Configured `dump_binary`, staging, log, and secret paths must be clean absolute
paths. Local validation rejects a symbolic link in any component of those
paths. Secret targets must be regular files; secret symlinks are not followed.
The cron manager likewise refuses a symlink, non-regular file, or multiply
hard-linked file at `/etc/cron.d/ezdbbackup`.

### Secret sources

MySQL accepts either `password` or `password_file`, never both. Omitting both
uses whatever authentication the `mysqldump` process already has, such as an
operator-managed login path or option file, for actual backups. Connectivity
probes start `mysqldump` with `--no-defaults` for safety, so authentication kept
only in a regular option file is not available to the probe. Configure a job
password source (or a supported MySQL login path) when using `--connectivity`.

S3 has literal and `_file` forms for `access_key_id`, `secret_access_key`, and
the optional `session_token`. Literal and file forms of the same value are
mutually exclusive. An explicit access key ID and secret access key must be
configured together, and a session token requires both. File paths must be
absolute. Trailing CR/LF characters are removed when a secret file is read.

When explicit S3 credentials are absent, ezdbbackup uses the AWS SDK default
credential chain, including environment variables, shared AWS configuration,
container credentials, and instance roles. Cron does not source an interactive
shell profile, so make the chosen credential source available to the configured
`run_as` account in the cron environment.

File-backed secrets are preferred because the YAML can remain non-secret.
Neither form is written to logs. Raw configuration and credential-bearing
process environments are not logged.

### `MYSQL_PWD` trade-off

ezdbbackup never adds a MySQL password to the argument vector and never creates
a MySQL option file. If configured, it removes any inherited `MYSQL_PWD` and
sets the resolved password only in the child `mysqldump` environment. AWS
access/secret/session credentials, profiles, and credential/config file
pointers are removed from that child environment; ordinary process variables
and the job's `MYSQL_PWD` remain available. The parent and unrelated processes
do not inherit the MySQL password.

This avoids exposing a password in command-line listings and avoids managing a
temporary option file, but environment inspection may still be possible to a
privileged user or to the same OS account on some systems. Restrict host access,
use a least-privilege MySQL backup account, and prefer a user-managed MySQL
login path or protected option file when that better matches the host's threat
model.

### Custom S3 endpoints

A custom endpoint must be a complete `http://` or `https://` URL. HTTPS keeps
normal certificate verification enabled; there is no insecure-TLS switch.
Plain HTTP is accepted for an explicitly configured private/development service
but validation emits a warning. Services without virtual-hosted bucket routing
usually need path-style addressing:

```yaml
s3:
  bucket: example-database-backups
  prefix: mysql/production
  region: us-east-1
  endpoint: https://objects.example.invalid
  force_path_style: true
  access_key_id_file: /etc/ezdbbackup/secrets/s3-access-key-id
  secret_access_key_file: /etc/ezdbbackup/secrets/s3-secret-access-key
```

Uploads use the AWS SDK retry behavior. Because the gzip is completely staged
before upload, an upload retry does not rerun `mysqldump`.

## Commands and exit codes

Options may appear before or after the job name.

```text
ezdbbackup backup <job> [--config <path>] [--debug]
ezdbbackup backup --all [--config <path>] [--debug]
ezdbbackup validate [<job> | --all] [--connectivity] [--config <path>] [--debug]
ezdbbackup cron install [--config <path>]
ezdbbackup cron show
ezdbbackup cron remove
ezdbbackup version
```

`backup <job>` requires that named job to exist and be enabled. `backup --all`
runs enabled jobs sequentially in lexical job-name order, continues after an
individual failure, prints a per-job summary, and fails overall if any job
failed. Disabled jobs are excluded from `backup --all`.

`validate` with no selector is equivalent to `validate --all`; it includes
disabled jobs so problems are caught before enabling them. An explicit job
validates only that job. `--debug` enables per-invocation debug logging for
backup; validation accepts it for command-line consistency but does not
initialize log files.

Exit categories are stable:

| Code | Meaning |
| ---: | --- |
| `0` | The requested operation succeeded (warnings are allowed). |
| `1` | A backup runtime operation failed, including logging initialization or one or more selected jobs. |
| `2` | Command usage, configuration, selection, or validation failed. |
| `3` | Cron installation, display, or removal failed. |

Errors and progress printed to the terminal are concise text. Operational
backup details are JSON Lines in the configured log directory.

## Validation and execution identity

Default validation is local and side-effect free. It validates the complete
schema and cron-safe paths; checks `run_as`; verifies the configured dump
binary is regular, traversable and executable; invokes it with `--version`;
checks staging/log directory writability; and checks file-backed secrets. A
missing staging or log directory is not created: validation checks whether
`run_as` could create it beneath the nearest existing parent.

`--connectivity` adds a no-data/no-DDL MySQL probe and S3 `HeadBucket`. MySQL
and S3 are reported separately. `HeadBucket` can be denied by a policy that
still permits uploads, so that error is diagnostic rather than proof that an
upload can never work.

> **Important identity behavior:** filesystem permissions are evaluated from
> metadata for each job's configured `run_as`, but the actual
> `mysqldump --version` command and both connectivity probes run as the OS user
> invoking ezdbbackup. v1 does not impersonate `run_as`. With
> `--connectivity`, a different invoking user produces a warning. To reproduce
> scheduled-job behavior, retain the same selector and configuration path and
> run exactly the corresponding form:
>
> `sudo -u <run_as> ezdbbackup validate [job|--all] --connectivity --config /absolute/path/to/config.yml`
>
> For example:
>
> `sudo -u ezdbbackup ezdbbackup validate production --connectivity --config /etc/ezdbbackup/config.yml`
>
> `sudo -u ezdbbackup ezdbbackup validate --all --connectivity --config /etc/ezdbbackup/config.yml`

The selected account must also have access to any AWS credential source being
tested. `sudo` may intentionally discard environment credentials; account for
that when comparing manual and cron validation.

Recommended rollout:

```sh
sudo -u ezdbbackup ezdbbackup validate --all --config /etc/ezdbbackup/config.yml
sudo -u ezdbbackup ezdbbackup validate --all --connectivity --config /etc/ezdbbackup/config.yml
sudo -u ezdbbackup ezdbbackup backup production --config /etc/ezdbbackup/config.yml
sudo ezdbbackup cron install --config /etc/ezdbbackup/config.yml
sudo ezdbbackup cron show
```

## Cron management

`cron install` validates every configured job, resolves the current executable
and config arguments to absolute paths, and atomically writes enabled jobs in
lexical order to the fixed path `/etc/cron.d/ezdbbackup`. Run install/remove as
an account permitted to manage `/etc/cron.d` (normally root). The installed file
has mode `0644`; under normal root invocation its ownership is `root:root`.

The filename is deliberately `ezdbbackup`, with no dot. Many cron daemons
ignore `/etc/cron.d` filenames containing a dot. Do not rename it to
`ezdbbackup.cron`.

The managed file starts with a generated-file warning and the exact ownership
marker:

```text
# ezdbbackup-managed: v1
```

Install, show, and remove refuse to act on an existing unmarked file. Install
also refuses unsafe destination links and replaces a marked file atomically.
Remove succeeds if the schedule is already absent and never touches unrelated
crontabs. `cron show` prints the exact marked file. The generated file uses
`/bin/sh`, a fixed system `PATH`, each job's five-field schedule and `run_as`,
and a command equivalent to:

```text
<absolute-binary> backup <job> --config <absolute-config>
```

Re-run `cron install` after changing schedules, enabled flags, the binary path,
or config path. There is no cron entry for disabled jobs and no `logrotate`
entry.

## Logs and rotation

Each log line is one JSON object with these core fields:

- `timestamp`: UTC timestamp;
- `level`: `debug`, `info`, `warn`, or `error`;
- `message`: human-readable event summary;
- `command`: the CLI command;
- optional `job` and `stage`; and
- flat structured context such as `duration`, `error`, `object_key`, or `size`.

Secret-like structured keys and resolved secret text in backup failures are
redacted. Error events go only to `error.log`; info and warning events go only
to `info.log`; debug events go to `debug.log` only when `logging.debug: true`
or backup's `--debug` flag enables debug mode. Required files are initialized
before a dump starts, and a logging initialization failure prevents the dump.

Rotation is built into the binary. Before a log write, it uses per-log
inter-process locks, rotates at `max_size_mb`, retains at most `max_files`,
removes rotations older than `max_age_days`, and optionally compresses rotated
files according to `compress`. The example spells out all four settings so the
operations policy is explicit; omitted settings receive the defaults described
above. Do not configure system `logrotate` for these files: two rotation
owners can race or defeat the built-in retention policy.

## Staging, cleanup, and object keys

Each job first writes one gzip file named like `ezdbbackup-*.sql.gz` in its
`temp_dir`. The file is mode `0600` and is complete before S3 upload starts.
Allow enough free space for the largest complete compressed dump plus filesystem
overhead. `backup --all` is sequential, so jobs do not intentionally stage in
parallel within one process; separate manual/cron invocations can still overlap.

The staged file is removed after successful upload and on dump, compression,
finalization, client creation, and upload failure paths. A partial staging file
is removed when staging fails. A cleanup failure is logged; if it is the only
failure, the job fails. Monitor the staging directory for residue after crashes
or forced termination, which cannot execute process cleanup handlers.

Object keys use the backup start time in UTC with nanosecond precision:

```text
<prefix>/<job>/<YYYY>/<MM>/<DD>/<job>-<YYYYMMDD>T<HHMMSS>.<nanoseconds>Z.sql.gz
```

For example:

```text
mysql/production/production/2026/08/10/production-20260810T021500.123456789Z.sql.gz
```

Leading and repeated `/` characters in `prefix` are normalized. When the
prefix is empty, its segment and separator are omitted.

## Build and test

Go 1.26 is required to build from source.

```sh
# Development/static-compatible build (prints version "dev").
CGO_ENABLED=0 go build -trimpath -o ezdbbackup ./cmd/ezdbbackup

# Unit tests, race detector, and static analysis.
go test ./...
go test -race ./...
go vet ./...

# Tagged integration test against LocalStack.
docker compose -f test/compose.yml up -d --wait
AWS_ACCESS_KEY_ID=test \
AWS_SECRET_ACCESS_KEY=test \
AWS_REGION=us-east-1 \
EZDBBACKUP_TEST_S3_ENDPOINT=http://127.0.0.1:4566 \
  go test -tags=integration ./test/e2e -v
docker compose -f test/compose.yml down -v
```

To reproduce a release-style version locally:

```sh
CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w -X github.com/ezgamehost/ezdbbackup/internal/buildinfo.Version=v1.2.3" \
  -o ezdbbackup ./cmd/ezdbbackup
./ezdbbackup version
# ezdbbackup v1.2.3
```
