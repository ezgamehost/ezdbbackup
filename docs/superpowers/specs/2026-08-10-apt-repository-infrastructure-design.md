# EZ Game Host APT Repository Infrastructure Design

Date: 2026-08-10
Status: Approved for planning

## Summary

EZ Game Host will operate a centralized, signed Debian APT repository at `https://packages.ezghcloud.com/apt`. A new public GitHub repository, `ezgamehost/apt-repo-infra`, will own the Cloudflare Worker, repository-generation code, publication workflow, signing policy, and a reusable GitHub Action that application repositories can invoke to publish `.deb` release assets.

Cloudflare R2 will be the private object store. A thin Worker on `packages.ezghcloud.com` will be the only public read path. Publishing will occur only in the infrastructure repository so application repositories never receive the APT signing key or Cloudflare credentials.

The canonical company name in user-facing content is **EZ Game Host**. Lowercase identifiers such as `ezgamehost`, `ezdbbackup`, and `packages.ezghcloud.com` remain unchanged where spaces are not permitted.

## Goals

- Install EZ Game Host software through a normal signed APT source rather than direct `.deb` downloads.
- Support `amd64` and `arm64` packages in suite `stable`, component `main`.
- Centralize signing, repository metadata, Cloudflare credentials, publication serialization, and audit history.
- Give application repositories a small, versioned publishing action.
- Publish `ezdbbackup` as the first package without weakening its existing release gates.
- Make partial publication fail safely and make repeated identical publication idempotent.
- Leave room for additional EZ Game Host packages without changing users' APT source configuration.

## Non-goals

- Hosting RPM, Alpine, container, or language-specific repositories in the first version.
- Supporting Debian source packages.
- Providing multiple suites, staged promotion, package deletion, or automatic retention pruning.
- Installing a live ezdbbackup configuration, cron file, log directory, or service account from the package.
- Operating an unauthenticated upload API in the Worker.

## Chosen approach and alternatives

### Chosen: central publisher with a dispatching shared action

Application repositories create and verify their `.deb` files, attach them to a GitHub release, and invoke a shared action from `apt-repo-infra`. The action verifies the local packages and sends an authenticated `repository_dispatch` request containing immutable release asset references and SHA-256 digests. A workflow in `apt-repo-infra` downloads and verifies those assets, updates the APT repository, signs it, and uploads it to R2.

This keeps privileged credentials in one repository and provides one serialized publication queue for every package.

### Rejected: publish directly from every application repository

This is simpler initially, but it distributes the archive signing key and Cloudflare credentials, makes organization-wide concurrency difficult, and enlarges the trusted publisher set.

### Rejected: direct public R2 bucket without a Worker

This removes Worker code but gives less control over methods, path validation, content types, cache policy, landing-page instructions, and public bypasses. The private-bucket Worker design is a small amount of code for materially clearer behavior.

## Repositories and ownership

### `ezgamehost/apt-repo-infra`

The new public repository owns:

- Worker source, tests, and Wrangler configuration.
- Repository generation and publication scripts.
- The public archive key and documented fingerprint.
- A composite action at `.github/actions/publish/action.yml`.
- CI, Worker deployment, repository publication, and smoke-test workflows.
- Operator and package-consumer documentation.

The default branch will be protected by required CI after the initial bootstrap if repository permissions allow it. Shared-action releases will use immutable Git tags such as `v1.0.0`; consumers will pin the resolved commit SHA and record the version in a comment.

### Application repositories

Each application repository remains responsible for building and testing its binaries and constructing valid `.deb` files. It calls the shared action only after all application release gates pass.

For `ezdbbackup`, the existing tag workflow will additionally create one package per architecture and then invoke the shared publisher after the GitHub release exists.

## Package format for ezdbbackup

The package metadata will use:

- Package: `ezdbbackup`
- Maintainer: `EZ Game Host Support <support@ezgamehost.com>`
- Section: `admin`
- Priority: `optional`
- Architecture: `amd64` or `arm64`
- Homepage: the ezdbbackup GitHub repository
- Depends: `ca-certificates`
- Recommends: `default-mysql-client`, `cron`

The package will contain:

- `/usr/bin/ezdbbackup`
- `/usr/share/doc/ezdbbackup/README.md`
- `/usr/share/doc/ezdbbackup/examples/config.yml`
- Standard Debian copyright and changelog documentation where available

It will not create `/etc/ezdbbackup/config.yml`, `/etc/cron.d/ezdbbackup.schedule`, log files, a system user, or MySQL option files. Those resources depend on the operator's selected `run_as` identity and remain explicit runtime configuration.

Package builds will use root ownership in the archive without requiring a root build process. Tests will inspect package control metadata, paths, modes, owners, architecture, and the embedded binary version.

## Repository layout

The public namespace will be:

```text
/
  index.html
  keys/ez-game-host-archive-keyring.asc
  apt/
    dists/stable/InRelease
    dists/stable/Release
    dists/stable/Release.gpg
    dists/stable/main/binary-amd64/Packages
    dists/stable/main/binary-amd64/Packages.gz
    dists/stable/main/binary-amd64/by-hash/SHA256/<digest>
    dists/stable/main/binary-arm64/Packages
    dists/stable/main/binary-arm64/Packages.gz
    dists/stable/main/binary-arm64/by-hash/SHA256/<digest>
    pool/main/e/ezdbbackup/ezdbbackup_<version>_<architecture>.deb
```

The Release metadata will identify:

- Origin: `EZ Game Host`
- Label: `EZ Game Host`
- Suite/Codename: `stable`
- Components: `main`
- Architectures: `amd64 arm64`
- Acquire-By-Hash: `yes`

Previously published versions are retained. Publishing the same package, version, architecture, and digest is a successful no-op. Reusing that tuple with different bytes is rejected.

## Shared publishing action

The composite action accepts:

- Release tag.
- Newline-separated `.deb` paths.
- The caller's GitHub token for release-asset upload.
- A narrowly scoped infrastructure dispatch token.

It performs these steps:

1. Reject unsafe paths, malformed tags, unexpected file types, duplicate tuples, and packages outside the `ezgamehost` GitHub organization.
2. Inspect each archive using Debian tooling and require a safe package name, Debian version, and supported architecture.
3. Calculate SHA-256 and size.
4. Upload each archive to the existing GitHub release without overwriting an existing asset.
5. Resolve the uploaded assets through the GitHub API and confirm their names and sizes.
6. Dispatch a publication request to `ezgamehost/apt-repo-infra` containing only the source repository, tag, asset names, sizes, and digests.

The action never receives the archive signing key or Cloudflare credentials. The cross-repository token will be a fine-grained credential limited to dispatching workflows in `apt-repo-infra`, stored as an organization or selected-repository Actions secret.

## Central publication workflow

The infrastructure workflow runs under a repository-wide production concurrency group and does not cancel an in-progress publication. It:

1. Validates the dispatch schema and allowlisted organization.
2. Downloads public assets from the specified immutable GitHub release.
3. Confirms size, SHA-256, Debian metadata, package filename, and architecture.
4. Fetches the current repository state from R2 into a clean temporary workspace.
5. Rejects conflicting package tuples and installs new packages under immutable pool paths.
6. Regenerates deterministic `Packages` and compressed indexes for both architectures.
7. Writes immutable `by-hash` indexes before publishing their references.
8. Generates `Release`, clearsigns `InRelease`, and creates detached `Release.gpg` using the archive key.
9. Verifies signatures and all Release checksums locally.
10. Uploads immutable packages and by-hash indexes first, canonical indexes second, and signed Release metadata last.
11. Runs HTTP and isolated APT-client smoke tests through `packages.ezghcloud.com`.

Publication aborts on the first validation, signing, upload, or smoke-test failure. A signed release is never uploaded until every referenced object is already present. Since clients use by-hash indexes, they retain a consistent path while canonical metadata changes.

## Signing and trust

The archive signer will be:

`EZ Game Host APT Repository <support@ezgamehost.com>`

A dedicated archive key will be generated for this repository. The public key and full fingerprint will be committed and served under `/keys/`. The encrypted private key and passphrase will exist only as protected GitHub Actions secrets in `apt-repo-infra` and will be used only by the production publication environment.

Installation instructions will use `/etc/apt/keyrings` and a deb822 `.sources` file with an explicit `Signed-By` path. They will show the expected fingerprint before enabling the source. Deprecated `apt-key` instructions will not be provided.

## Cloudflare architecture

- R2 bucket: private and inaccessible through `r2.dev`.
- Worker custom domain: `packages.ezghcloud.com`.
- Worker binding: direct R2 binding; no S3 credentials at request time.
- Public methods: `GET` and `HEAD`; all others return `405`.
- Root path: a small installation page.
- Object paths: exact key lookup only; no bucket listing.
- Path handling: reject invalid encoding, control bytes, backslashes, dot segments, and noncanonical paths.
- Range and conditional requests: forwarded to R2 where supported.
- `.deb` and by-hash objects: long-lived public immutable caching.
- `InRelease`, `Release`, `Release.gpg`, `Packages`, and `Packages.gz`: no-store or revalidation-required caching.
- Security headers: nosniff, restrictive content security policy on the landing page, and no MIME inference from untrusted data.

The deployment workflow will use scoped Cloudflare account and zone credentials. Initial provisioning creates the R2 bucket, deploys the Worker, and attaches the existing `ezghcloud.com` zone custom domain. Subsequent deployments update only declared infrastructure.

## Secrets and permissions

`apt-repo-infra` requires protected secrets for:

- Cloudflare API token.
- Cloudflare account ID and zone ID if not encoded in configuration.
- ASCII-armored archive private key.
- Archive key passphrase.

Application repositories require only the fine-grained infrastructure dispatch token in addition to their built-in GitHub token.

Every workflow starts with `permissions: {}` and grants the minimum job-level permissions. Third-party Actions are pinned to immutable commit SHAs. Secrets are never accepted as command-line arguments, logged, embedded in artifacts, or exposed to pull-request workflows.

## CI and testing

Infrastructure CI will include:

- Worker unit tests covering GET, HEAD, ranges, cache headers, content types, missing objects, forbidden methods, path traversal, encoding edge cases, and terminal-safe errors.
- Script tests with controlled Debian, GPG, GitHub, and R2 tools covering success and fail-closed behavior.
- Repository fixture tests validating signatures, checksums, architectures, by-hash objects, idempotency, and conflict rejection.
- Composite-action tests covering input validation, asset verification, non-overwrite behavior, digest binding, and dispatch payloads.
- Shell syntax checks, actionlint, formatting, type checking, dependency audit, and Worker dry-run deployment.
- An isolated Debian/Ubuntu container that installs the public key and source, runs `apt-get update`, installs the package, and verifies `ezdbbackup version`.

The ezdbbackup workflow retains its unit, vet, race, static-build, LocalStack, checksum, and release gates. New `.deb` tests run before GitHub release or APT dispatch.

## Error handling and recovery

- Invalid or conflicting inputs fail before repository state changes.
- Upload order prevents a newly signed Release file from referencing absent objects.
- Immutable package and by-hash objects make retries safe.
- The central concurrency group prevents two publisher workflows from generating metadata concurrently.
- Failed publication retains logs and a manifest artifact without retaining private key material.
- A manual rebuild workflow can reconstruct all metadata from retained pool packages.
- Package deletion and signing-key rotation require separate reviewed procedures and are not implicit side effects of normal publication.

## Consumer installation flow

The landing page and README will instruct users to:

1. Download the archive public key into `/etc/apt/keyrings`.
2. Verify its documented fingerprint.
3. Add a deb822 source using `https://packages.ezghcloud.com/apt`, suite `stable`, component `main`, architectures `amd64 arm64`, and explicit `Signed-By`.
4. Run `apt-get update` and `apt-get install ezdbbackup`.

No unauthenticated installation script will modify the machine automatically.

## Rollout and success criteria

1. Create and secure `ezgamehost/apt-repo-infra`.
2. Generate and publish the archive public key while protecting the private key.
3. Provision the private R2 bucket and deploy the Worker custom domain.
4. Publish an empty signed repository and verify `apt-get update`.
5. Add ezdbbackup `.deb` construction and the shared action call to its release workflow.
6. Publish a test version, install it on both architectures where runners are available, and verify its embedded version.
7. Document operational recovery and future-package onboarding.

The work is complete when the infrastructure and ezdbbackup repositories are pushed, all CI is green, `packages.ezghcloud.com` serves a valid signed repository, and a clean supported host can install ezdbbackup using `apt-get install ezdbbackup`.

## External prerequisites

Creating GitHub repositories and code is possible with the currently authenticated GitHub CLI. Live Cloudflare provisioning requires a scoped Cloudflare API token and account identifiers. If those credentials are not already available through an authenticated Wrangler session or GitHub secrets, code and CI can be completed first, but live deployment remains blocked until the account owner supplies them.
