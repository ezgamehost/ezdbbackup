# EZ Game Host APT Repository Infrastructure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Build, deploy, and verify a centralized signed APT repository at packages.ezghcloud.com, then publish ezdbbackup through a reusable action owned by ezgamehost/apt-repo-infra.

**Architecture:** A public infrastructure repository owns a private-R2-backed Cloudflare Worker, deterministic APT metadata generation, signing, serialized publication, and a composite dispatch action. Application repositories build verified Debian packages and request central publication by immutable GitHub release reference, keeping Cloudflare and signing credentials out of callers.

**Tech Stack:** Cloudflare Workers and R2, TypeScript, Vitest/Miniflare, Bash, Debian package tools, GnuPG, GitHub Actions, Wrangler, and Go workflow tests.

## Global Constraints

- User-facing company name is exactly EZ Game Host; lowercase technical slugs remain where spaces are impossible.
- APT base URL is https://packages.ezghcloud.com/apt, suite stable, component main.
- Architectures are exactly amd64 and arm64.
- R2 remains private and production never uses r2.dev.
- Signing and Cloudflare secrets exist only in the infrastructure repository.
- Application releases retain all existing verification gates.
- Workflows default to empty permissions and pin third-party actions before production.
- Normal publication never deletes an existing package version.

---

### Task 1: Bootstrap the infrastructure repository

**Files:**
- Create in ezgamehost/apt-repo-infra: .gitignore, README.md, AGENTS.md
- Create: package.json, package-lock.json, tsconfig.json, vitest.config.ts, wrangler.toml
- Create: scripts/check.sh, test/repository_layout_test.sh

**Interfaces:**
- Consumes: authenticated GitHub CLI and the approved design.
- Produces: public repository with npm test, npm run typecheck, and bash scripts/check.sh entry points.

- [ ] **Step 1: Create and clone the public repository**

    gh repo create ezgamehost/apt-repo-infra --public \
      --description "Signed APT repository infrastructure for EZ Game Host" --clone

Expected: gh repo view reports visibility PUBLIC.

- [ ] **Step 2: Write and run a RED layout test**

The test requires package.json, TypeScript configuration, Wrangler configuration, README, check script, and exact EZ Game Host branding. It rejects EZGameHost and EZ GameHost in user-facing files.

    bash test/repository_layout_test.sh

Expected: FAIL naming the first absent file.

- [ ] **Step 3: Add the locked TypeScript/Workers toolchain**

Use scripts test=vitest run, typecheck=tsc --noEmit, and deploy:dry-run=wrangler deploy --dry-run. Configure Worker ez-game-host-apt, compatibility date 2026-08-10, R2 binding PACKAGES, bucket ez-game-host-apt, and custom domain packages.ezghcloud.com.

- [ ] **Step 4: Verify GREEN and commit**

    npm install
    bash test/repository_layout_test.sh
    npm run typecheck
    git add .
    git commit -m "chore: bootstrap apt repository infrastructure"
    git push -u origin main

---

### Task 2: Implement the private-R2 Worker

**Files:**
- Create: src/index.ts, src/paths.ts, src/headers.ts
- Create: test/worker.test.ts
- Modify: wrangler.toml, README.md

**Interfaces:**
- Consumes: Env with PACKAGES R2Bucket and HTTP requests.
- Produces: fetch(request, env), parseObjectKey(url), and headersFor(key, object).

- [ ] **Step 1: Write RED Worker tests**

Cover root HTML and CSP, GET, HEAD, 404, byte ranges, ETag conditionals, MIME, immutable package/by-hash caching, mutable metadata no-store, nosniff, and no listing. Require POST, PUT, DELETE, and PATCH to return 405 with Allow: GET, HEAD. Reject /../secret, /%2e%2e/secret, /apt/%5csecret, and /apt//double.

    npm test -- test/worker.test.ts

Expected: FAIL because the handler is absent.

- [ ] **Step 2: Implement strict paths, reads, and headers**

Reject invalid encoding, control bytes, backslashes, empty interior segments, dot segments, and keys outside apt/ or keys/. Map MIME types from a fixed allowlist. Apply public max-age=31536000 immutable only to Debian package and by-hash objects; apply no-store to Release and Packages metadata. Set X-Content-Type-Options: nosniff. Forward range and conditional headers to R2, return 416 for unsatisfied ranges, and omit bodies for HEAD.

- [ ] **Step 3: Verify GREEN and commit**

    npm test
    npm run typecheck
    npm run deploy:dry-run
    git add src test wrangler.toml README.md
    git commit -m "feat: serve apt repository from private r2"
    git push

---

### Task 3: Generate and sign APT metadata

**Files:**
- Create: scripts/publish-repository.sh
- Create: scripts/lib/validate-package.sh, scripts/lib/generate-metadata.sh, scripts/lib/upload-repository.sh
- Create: test/publish_repository_test.sh, test/fixtures/make-package.sh
- Create: config/apt-ftparchive.conf

**Interfaces:**
- Consumes: downloaded package manifest, repository directory, temporary GNUPGHOME, signing fingerprint, passphrase descriptor, and R2 destination.
- Produces: pool objects, Packages indexes, by-hash copies, Release, InRelease, Release.gpg, and ordered uploads.

- [ ] **Step 1: Write and run RED validation tests**

Build real tiny fixtures with dpkg-deb --root-owner-group --build. Cover valid amd64/arm64, idempotent repeat, conflicting same tuple, invalid name/version/architecture, filename mismatch, size/digest mismatch, symlink, non-regular file, duplicates, control bytes, and a source repository outside the ezgamehost organization.

    bash test/publish_repository_test.sh validation

Expected: FAIL because publisher scripts are absent.

- [ ] **Step 2: Implement package validation and immutable pool paths**

Read control fields with dpkg-deb -f. Require package names to match lowercase Debian package grammar, architecture exactly amd64 or arm64, exact manifest digest and size, and filename package_version_architecture.deb. Accept a repeated tuple only when bytes match.

- [ ] **Step 3: Write RED metadata, signature, and upload tests**

Require sorted multiversion indexes, deterministic gzip without timestamps, both architectures, Acquire-By-Hash: yes, Origin and Label EZ Game Host, valid gpgv checks, correct SHA-256 Release entries, and upload order: packages, by-hash, canonical indexes, Release files, InRelease last. Inject failure at every stage and assert later stages do not run.

- [ ] **Step 4: Implement deterministic indexes and signing**

Generate each architecture with dpkg-scanpackages --multiversion --arch, gzip -n, and SHA-256 by-hash copies. Generate fixed Release fields with apt-ftparchive. Sign by exact fingerprint using batch mode, loopback pinentry, and a passphrase file descriptor; never include the passphrase in argv or logs.

- [ ] **Step 5: Implement exact ordered R2 uploads**

Upload explicit files rather than a broad sync. Set immutable cache control only for package and by-hash paths; set no-store for mutable metadata. Reject empty, root-like, or malformed destinations.

- [ ] **Step 6: Verify GREEN and commit**

    bash test/publish_repository_test.sh all
    bash -n scripts/publish-repository.sh scripts/lib/*.sh test/*.sh
    git add scripts test config
    git commit -m "feat: generate and sign apt repository metadata"
    git push

---

### Task 4: Add the reusable dispatch action and workflows

**Files:**
- Create: .github/actions/publish/action.yml, .github/actions/publish/publish.sh
- Create: .github/workflows/ci.yml, .github/workflows/publish.yml
- Create: .github/workflows/deploy.yml, .github/workflows/rebuild.yml
- Create: scripts/import-signing-key.sh, scripts/smoke-test.sh
- Create: test/publish_action_test.sh, test/workflows_test.sh
- Modify: scripts/check.sh, README.md

**Interfaces:**
- Action consumes release tag, newline-separated package paths, caller GitHub token, and infrastructure dispatch token.
- Action produces verified release assets and payload with source repository, release tag, asset name, size, and SHA-256.
- Central workflow consumes apt-package-publish and produces one serialized signed R2 publication.

- [ ] **Step 1: Write and run RED action tests**

Use fake dpkg-deb, sha256sum, and gh executables with structured argv logs. Cover empty input, unsafe tags, glob injection, newline/control bytes, symlinks, unsupported architectures, duplicates, existing assets, callers outside ezgamehost, upload failure, asset re-resolution mismatch, dispatch failure, and secret redaction.

    bash test/publish_action_test.sh

Expected: FAIL because the action is absent.

- [ ] **Step 2: Implement the composite action**

Pass tokens through environment only. Parse newline-delimited paths without eval, validate each regular file and Debian tuple, upload without overwrite, resolve each asset again, and build JSON only with jq typed arguments. Dispatch apt-package-publish to ezgamehost/apt-repo-infra only after every asset is verified.

- [ ] **Step 3: Write and run RED workflow-policy tests**

Require exact triggers, empty default permissions, bounded timeouts, production concurrency apt-repository-production with cancellation disabled, protected environment, pinned actions, no secrets interpolated into shell code, temporary GNUPGHOME cleanup, and redacted manifest artifacts.

- [ ] **Step 4: Implement CI, deployment, publication, and rebuild**

CI runs npm, TypeScript, Worker, shell, ShellCheck, and actionlint checks. Deployment idempotently creates the R2 bucket, disables public r2.dev, and deploys the custom domain. Publication validates payload before download, verifies release redirect host, size, digest, and package metadata, imports the exact signing fingerprint into a temporary keyring, fetches current R2 state, publishes, and runs smoke tests. Rebuild regenerates metadata only from retained pool packages.

- [ ] **Step 5: Verify GREEN, commit, and tag**

    bash test/publish_action_test.sh
    bash test/workflows_test.sh
    bash scripts/check.sh
    npm test
    npm run typecheck
    npm run deploy:dry-run
    git add .
    git commit -m "ci: publish and deploy signed apt repository"
    git push
    git tag -a v1.0.0 -m "EZ Game Host APT publisher v1.0.0"
    git push origin v1.0.0

Consumers pin the resulting 40-character commit SHA, not the version label.

---

### Task 5: Build Debian packages in ezdbbackup

**Files:**
- Modify: scripts/package-release.sh, test/workflow/package_release_test.go
- Create: packaging/debian/copyright, packaging/debian/changelog.template
- Modify: README.md

**Interfaces:**
- Consumes: each already verified static binary and release tag.
- Produces: tarballs, ezdbbackup_version_amd64.deb, ezdbbackup_version_arm64.deb, and one SHA256SUMS covering every artifact.

- [ ] **Step 1: Extend package tests and verify RED**

Require both Debian package names. Inspect each with dpkg-deb -f and dpkg-deb -c. Assert Package, version without v, architecture, Maintainer EZ Game Host Support <support@ezgamehost.com>, Section admin, Priority optional, Depends ca-certificates, Recommends default-mysql-client and cron, Homepage, root ownership, executable mode, documentation, and absence of /etc, cron, logs, user creation, or MySQL option files.

    go test ./test/workflow -run TestPackageRelease -count=1

Expected: FAIL because Debian packages are absent.

- [ ] **Step 2: Build packages from the exact verified binaries**

Create a mode-0700 temporary package root, install the binary at usr/bin/ezdbbackup mode 0755, docs mode 0644, and generated control metadata. Run dpkg-deb --root-owner-group --build to the architecture-specific output. Inspect every result before checksumming. Extend the controlled-tool harness with build, metadata, content, owner/mode, and checksum failure injection.

- [ ] **Step 3: Verify GREEN and commit**

    go test ./test/workflow -run TestPackageRelease -count=1
    go test ./... -count=1
    go test -race ./... -count=1
    go vet ./...
    git add scripts/package-release.sh test/workflow/package_release_test.go packaging README.md
    git commit -m "feat: package ezdbbackup for debian"
    git push origin main

---

### Task 6: Integrate ezdbbackup with the shared publisher

**Files:**
- Modify: .github/workflows/release.yml, test/workflow/release_test.go, README.md

**Interfaces:**
- Consumes: immutable shared-action SHA, dist Debian packages, GitHub token, and APT_REPOSITORY_DISPATCH_TOKEN.
- Produces: release package assets and a central publication dispatch only after every existing gate.

- [ ] **Step 1: Write and run RED workflow tests**

Require GitHub release publication to include checksummed Debian packages. Require an apt-publish job that needs publish, has bounded permissions and timeout, invokes the shared action by full commit SHA, passes exactly two package paths, and obtains the dispatch credential only from the named secret.

    go test ./test/workflow -run TestRelease -count=1

Expected: FAIL because the job is absent.

- [ ] **Step 2: Add the shared-action job**

Download the verified artifact, check SHA256SUMS, derive the package version in an audited shell step, and invoke the pinned action. The action uploads Debian assets to the existing release and dispatches central publication.

- [ ] **Step 3: Run the complete matrix and commit**

    test -z "$(gofmt -l .)"
    go test ./... -count=1
    go test -race ./... -count=1
    go vet ./...
    bash -n scripts/package-release.sh
    git diff --check
    git add .github/workflows/release.yml test/workflow/release_test.go README.md
    git commit -m "ci: publish releases to ez game host apt"
    git push origin main

---

### Task 7: Provision, publish, and audit the live service

**Files:**
- Create in infrastructure repository: docs/operations.md, docs/onboarding.md
- Modify in infrastructure repository: README.md
- Modify in ezdbbackup: README.md

**Interfaces:**
- Consumes: scoped Cloudflare credentials, protected GitHub environments and secrets, dedicated archive key, and an ezdbbackup release.
- Produces: live signed repository and verified apt-get install ezdbbackup.

- [ ] **Step 1: Generate and protect the archive key**

Use a temporary mode-0700 GNUPGHOME to generate a passphrase-protected RSA-4096 key named EZ Game Host APT Repository <support@ezgamehost.com>. Export the public key and fingerprint, write private material directly to protected GitHub secrets without terminal output, verify only secret names, then destroy the temporary directory.

- [ ] **Step 2: Configure production credentials**

Create protected environment packages-production; set Cloudflare account, zone, and scoped token secrets there. Set the narrowly scoped infrastructure-dispatch credential for ezdbbackup. Verify configuration through GitHub metadata APIs without reading values.

- [ ] **Step 3: Deploy and publish an empty repository**

Run deployment and rebuild workflows and watch them with gh run watch --exit-status. Prove DNS and HTTPS, root instructions, 405 for writes, disabled r2.dev, valid InRelease, and clean Debian/Ubuntu apt-get update.

- [ ] **Step 4: Publish and install ezdbbackup**

Create or select the next valid release tag only after main CI is green. Watch application release and central publication. Confirm GitHub and R2 digests. On clean clients require command -v to resolve /usr/bin/ezdbbackup, exact embedded release version, and correct dpkg architecture. Use native or declared emulation for arm64.

- [ ] **Step 5: Document and perform the completion audit**

Document fingerprint verification, consumer setup, publish inputs, secret scopes, recovery rebuild, conflict handling, cache behavior, key rotation, and package onboarding. Search user-facing text for incorrect brand variants. Verify clean synchronized repositories, green GitHub checks, Worker headers/content, GPG signatures, Release hashes, and clean APT installs against every design success criterion.

- [ ] **Step 6: Commit and push final documentation**

    git add README.md docs
    git commit -m "docs: operate the ez game host apt repository"
    git push
