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
tool if `useradd` is unavailable. The configuration and secret files must be
regular, owned by root or the intended `run_as` user, readable by `run_as`, not
group-writable, and have no permissions for “other”; mode `0640` with the
service group is typical. The scheduled binary must likewise be root- or
`run_as`-owned and must not be writable by group or other users. Give `run_as`
traversal permission on every lexical and symlink-resolved parent directory.
Do not put a real secret on a command line or in shell history while
provisioning it.

The binary creates a missing staging directory with mode `0700`, a missing
single-user log directory with mode `0750`, and private log objects with exact
mode `0640`, but pre-creating them makes ownership explicit. A process cannot
change an existing directory's ownership, so cron's `run_as` user must already
be able to write both targets.

When enabled jobs use distinct OS identities, the global log directory must
already exist as a root-owned, setgid directory for a dedicated group shared by
all those users. It must grant that group read, write, and search permission and
must never be other-writable. For example:

```sh
sudo groupadd --system ezdbbackup-logs
sudo usermod -a -G ezdbbackup-logs backup-a
sudo usermod -a -G ezdbbackup-logs backup-b
sudo install -d -o root -g ezdbbackup-logs -m 2770 /var/log/ezdbbackup
```

In this layout active logs, stable locks, rotations, and gzip files use exact
mode `0660` and inherit the shared group. A missing directory is rejected for
multiple identities so one job cannot accidentally create a private directory
that blocks the others.

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

Configured binary, configuration, staging, log, and secret paths must be clean
absolute paths. Secure distribution-style symlinks are supported for binaries,
configuration, secrets, and the log target: validation resolves the final
target without following a final replacement, verifies its identity and type,
and checks traversal through both the lexical and resolved paths. Staging keeps
the stronger rule that every component must be symlink-free. Secret and
configuration targets must be regular, single-link files. The cron manager
likewise refuses a symlink, non-regular file, or multiply hard-linked file at
`/etc/cron.d/ezdbbackup`.

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

A custom endpoint must be a complete `http://` or `https://` URL with a valid
DNS/IP host. User information, queries, fragments, controls (including
percent-encoded controls), and invalid ports are rejected; an endpoint path
prefix is preserved. HTTPS keeps normal certificate verification enabled;
there is no insecure-TLS switch. Plain HTTP is accepted for an explicitly
configured private/development service but validation emits a warning. Services
without virtual-hosted bucket routing usually need path-style addressing:

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

Without a custom endpoint, bucket names use AWS's 3-63 character DNS-compatible
grammar and reserved AWS prefixes/suffixes are rejected. Custom endpoints use a
conservative single-segment ASCII bucket grammar (uppercase and `_` are also
accepted). Regions are bounded lowercase identifiers without empty components.
Prefixes must be valid terminal-safe UTF-8, contain no relative `.`/`..`
segments, and leave the generated object key within S3's 1024-byte limit.

Uploads use the AWS SDK retry behavior. Files up to 8 MiB use `PutObject`;
larger files use ordered multipart upload with seekable bounded parts. Part size
grows automatically when necessary to remain within S3's 10,000-part limit.
Every failure after multipart creation attempts `AbortMultipartUpload` with a
fresh 30-second context, including when the original request was canceled.
Because the gzip is completely staged before upload, an upload retry does not
rerun `mysqldump`.

Human-facing S3 errors contain only a fixed operation name plus vetted API code,
HTTP status, and request/host IDs only when the standard AWS endpoint boundary
can trust them. Custom endpoint response IDs are omitted and their API codes use
a fixed S3 allowlist. Raw endpoint messages, bodies, headers, URLs, signatures,
and credential material are not printed or logged; the wrapped SDK error remains
available to programmatic callers.

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
individual failure, prints each job's lifecycle progress immediately, then
prints one aggregate summary and fails overall if any job failed. Disabled jobs
are excluded from `backup --all`.

`backup` fails closed unless its effective OS user matches every selected
job's `run_as`; it never executes a configured dump binary with the invoking
user's extra privileges. Consequently, a manual `backup --all` can select only
jobs that share one OS identity. Cron schedules jobs independently under their
configured identities, so configurations with distinct `run_as` users remain
supported.

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

Errors and progress printed to the terminal are concise text, never JSON. Each
untrusted value is bounded to one physical line with terminal controls, bidi
controls, and invalid UTF-8 rendered visibly. Operational backup details are
JSON Lines in the configured log directory.

## Validation and execution identity

Default validation is local and side-effect free. It validates the complete
schema and cron-safe paths; checks `run_as`; proves that every enabled
`run_as` can traverse and execute both the configured dump binary and the
current ezdbbackup binary, and can traverse and read the effective
configuration; invokes executables with `--version`; checks staging/log
directory policy; and checks file-backed secrets. A
shared writable staging target or ancestor must be sticky (as `/tmp` normally
is), and existing path ancestors must be owned by `run_as` or root. A missing
staging or single-user log directory is not created: validation checks whether
`run_as` could create it beneath the nearest existing parent. A global log
directory shared by distinct identities must already exist with the setgid
group policy described above.

`--connectivity` adds a no-data/no-DDL MySQL probe and S3 `HeadBucket`. MySQL
and S3 are reported separately. `HeadBucket` can be denied by a policy that
still permits uploads, so that error is diagnostic rather than proof that an
upload can never work.

> **Important identity behavior:** filesystem permissions are evaluated for
> each job's configured `run_as`. Executable `--version` probes also run as
> `run_as` when validation is invoked by root; a same-user invocation keeps its
> current identity, and an unprivileged cross-user invocation fails closed
> instead of executing another user's binary. Connectivity probes still run as
> the OS user invoking ezdbbackup. With `--connectivity`, a different invoking
> user produces a warning. To reproduce scheduled-job behavior, retain the same
> selector and configuration path and run exactly the corresponding form:
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
Debug mode records curated stage diagnostics such as option counts and safe
booleans; it never records raw configuration, environment data, endpoints, or
credentials.

Rotation is built into the binary. During logger initialization and before each
log write, it uses stable per-family inter-process locks, rotates at
`max_size_mb`, retains at most `max_files` (between 1 and 1000), removes
rotations older than `max_age_days`, and optionally compresses rotated files
according to `compress`. Initialization performs this maintenance for error,
info, and enabled debug logs even when that run emits no event at a level. All
filesystem operations are directory-descriptor-relative, reject links and
unsafe replacements, and avoid overwriting unrelated entries. The example
spells out all four settings so the operations policy is explicit; omitted
settings receive the defaults described above. Do not configure system
`logrotate` for these files: two rotation owners can race or defeat the
built-in retention policy.

Every lifecycle log write is part of the job result. A start or transition log
failure stops the next dump/upload side effect; a completion log failure leaves
the uploaded S3 object in place but reports the job as failed. Cleanup and
failure-log errors are retained behind the earliest operational failure.

## Staging, cleanup, and object keys

Each job creates a unique mode-`0700` directory named like
`ezdbbackup-<random>` beneath `temp_dir`, then writes `backup.sql.gz` inside it
with mode `0600`. The file is complete before S3 upload starts. Allow enough
free space for the largest complete compressed dump plus filesystem overhead.
`backup --all` is sequential, so jobs do not intentionally stage in parallel
within one process; separate manual/cron invocations can still overlap.

Staging records the parent/work-directory and file device, inode, size, type,
mode, and link identity. Before upload, the file is reopened relative to the
recorded directory with symlink following disabled; only an exact regular,
single-link identity is accepted, and upload reads that verified descriptor.
Cleanup likewise removes only the original identity. If a path was replaced,
relinked, resized, or permission-broadened, the replacement is left untouched
and a cleanup safety error is reported.

The staged file and private work directory are removed after successful upload
and on dump, compression, finalization, client creation, and upload failure
paths. A partial staging file and its work directory are removed when staging
fails. A cleanup failure is logged; if it is the only failure, the job fails.
Monitor the staging directory for residue after crashes or forced termination,
which cannot execute process cleanup handlers.

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

# Tagged integration test against the pinned LocalStack image. Use a unique
# Compose project and always remove containers, networks, volumes, and orphans.
export COMPOSE_PROJECT_NAME="ezdbbackup-local-$$"
trap 'docker compose -f test/compose.yml down -v --remove-orphans' EXIT
docker compose -f test/compose.yml up -d --wait --wait-timeout 120
AWS_ACCESS_KEY_ID=test \
AWS_SECRET_ACCESS_KEY=test \
AWS_REGION=us-east-1 \
EZDBBACKUP_TEST_S3_ENDPOINT=http://127.0.0.1:4566 \
  go test -count=1 -tags=integration ./test/e2e -v
docker compose -f test/compose.yml down -v --remove-orphans
trap - EXIT
```

For a `v*` tag, the release workflow runs uncached unit tests, vet, race tests,
the complete tagged LocalStack suite, and the static `linux/amd64` and
`linux/arm64` build/package checks as separate gates. The publish job depends
on every gate, rechecks `SHA256SUMS`, and is the only job granted
`contents: write`; verification jobs cannot publish a release.

To reproduce a release-style version locally:

```sh
CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w -X github.com/ezgamehost/ezdbbackup/internal/buildinfo.Version=v1.2.3" \
  -o ezdbbackup ./cmd/ezdbbackup
./ezdbbackup version
# ezdbbackup v1.2.3
```
