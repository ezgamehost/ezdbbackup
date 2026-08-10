#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${repository_root}"

version="${GITHUB_REF_NAME:-}"
if [[ ! "${version}" =~ ^v[0-9A-Za-z][0-9A-Za-z._-]*$ ]]; then
  echo "unsupported release tag for artifact names: ${version}" >&2
  exit 1
fi
package_version="${version#v}"
if ! dpkg --validate-version "${package_version}" >/dev/null 2>&1; then
  echo "release tag does not contain a valid Debian version: ${version}" >&2
  exit 1
fi
release_epoch="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}"
if [[ ! "${release_epoch}" =~ ^[1-9][0-9]*$ ]]; then
  echo "release timestamp must be a positive integer: ${release_epoch}" >&2
  exit 1
fi
release_date="$(LC_ALL=C date -u -d "@${release_epoch}" '+%a, %d %b %Y %H:%M:%S +0000')"

require_deb_field() {
  local deb=$1
  local field=$2
  local expected=$3
  local actual
  actual="$(dpkg-deb -f "${deb}" "${field}")"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "package ${field} field is ${actual@Q}, expected ${expected@Q}: ${deb}" >&2
    exit 1
  fi
}

verify_deb_package() {
  local deb=$1
  local arch=$2
  local verified_binary=$3
  local expected_contents actual_contents extract_root control_root control_contents

  require_deb_field "${deb}" Package ezdbbackup
  require_deb_field "${deb}" Version "${package_version}"
  require_deb_field "${deb}" Architecture "${arch}"
  require_deb_field "${deb}" Maintainer "EZ Game Host Support <support@ezgamehost.com>"
  require_deb_field "${deb}" Section admin
  require_deb_field "${deb}" Priority optional
  require_deb_field "${deb}" Depends ca-certificates
  require_deb_field "${deb}" Recommends "default-mysql-client, cron"
  require_deb_field "${deb}" Homepage "https://github.com/ezgamehost/ezdbbackup"
  require_deb_field "${deb}" Description $'Static MySQL backup and S3 upload tool\n ezdbbackup runs named MySQL dump jobs, stores structured local logs, and\n uploads backup objects to Amazon S3 or compatible endpoints.'

  expected_contents="$(printf '%s\n' \
    $'./\tdrwxr-xr-x\troot/root' \
    $'./usr/\tdrwxr-xr-x\troot/root' \
    $'./usr/bin/\tdrwxr-xr-x\troot/root' \
    $'./usr/bin/ezdbbackup\t-rwxr-xr-x\troot/root' \
    $'./usr/share/\tdrwxr-xr-x\troot/root' \
    $'./usr/share/doc/\tdrwxr-xr-x\troot/root' \
    $'./usr/share/doc/ezdbbackup/\tdrwxr-xr-x\troot/root' \
    $'./usr/share/doc/ezdbbackup/README.md\t-rw-r--r--\troot/root' \
    $'./usr/share/doc/ezdbbackup/changelog.gz\t-rw-r--r--\troot/root' \
    $'./usr/share/doc/ezdbbackup/copyright\t-rw-r--r--\troot/root' \
    $'./usr/share/doc/ezdbbackup/examples/\tdrwxr-xr-x\troot/root' \
    $'./usr/share/doc/ezdbbackup/examples/config.yml\t-rw-r--r--\troot/root' \
    | LC_ALL=C sort)"
  actual_contents="$(dpkg-deb --contents "${deb}" | awk '{ print $NF "\t" $1 "\t" $2 }' | LC_ALL=C sort)"
  if [[ "${actual_contents}" != "${expected_contents}" ]]; then
    echo "package contents do not match required manifest: ${deb}" >&2
    diff -u <(printf '%s\n' "${expected_contents}") <(printf '%s\n' "${actual_contents}") >&2 || true
    exit 1
  fi

  control_root="${package_root}/verify_control_${arch}"
  install -d -m 0700 "${control_root}"
  dpkg-deb --control "${deb}" "${control_root}"
  control_contents="$(find "${control_root}" -mindepth 1 -printf '%P\t%y\t%m\n' | LC_ALL=C sort)"
  if [[ "${control_contents}" != $'control\tf\t644' ]]; then
    echo "package control archive contains unexpected files: ${deb}" >&2
    exit 1
  fi

  extract_root="${package_root}/verify_${arch}"
  install -d -m 0700 "${extract_root}"
  dpkg-deb --extract "${deb}" "${extract_root}"
  if ! cmp -s -- "${verified_binary}" "${extract_root}/usr/bin/ezdbbackup"; then
    echo "package binary differs from verified build: ${deb}" >&2
    exit 1
  fi
}

release_dist="${EZDBBACKUP_RELEASE_DIST_DIR:-dist}"
mkdir -p "${release_dist}"
if [[ -n "$(find "${release_dist}" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
  echo "release output directory must be empty: ${release_dist}" >&2
  exit 1
fi

package_root="$(mktemp -d)"
trap 'rm -rf -- "${package_root}"' EXIT

for arch in amd64 arm64; do
  package_dir="${package_root}/linux_${arch}"
  mkdir -p "${package_dir}"

  case "${arch}" in
    amd64) file_arch="x86-64" ;;
    arm64) file_arch="ARM aarch64" ;;
  esac

  binary="${package_dir}/ezdbbackup"
  CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" go build \
    -trimpath \
    -ldflags="-s -w -X github.com/ezgamehost/ezdbbackup/internal/buildinfo.Version=${version}" \
    -o "${binary}" \
    ./cmd/ezdbbackup

  file_output="$(file "${binary}")"
  echo "${file_output}"
  if [[ "${file_output}" != *"ELF 64-bit LSB executable"* ||
        "${file_output}" != *"${file_arch}"* ||
        "${file_output}" != *"statically linked"* ]]; then
    echo "release binary is not a static ${arch} Linux ELF executable: ${binary}" >&2
    exit 1
  fi

  set +e
  ldd_output="$(ldd "${binary}" 2>&1)"
  ldd_status=$?
  set -e
  echo "${ldd_output}"
  if (( ldd_status == 0 )) || [[ "${ldd_output}" == *"=>"* ]]; then
    echo "dynamic dependency reported for ${binary}" >&2
    exit 1
  fi
  if [[ "${ldd_output}" != *"not a dynamic executable"* && "${ldd_output}" != *"statically linked"* ]]; then
    echo "unexpected ldd result for ${binary} (status ${ldd_status})" >&2
    exit 1
  fi

  if [[ "${arch}" == "amd64" ]]; then
    version_output="$("${binary}" version)"
    if [[ "${version_output}" != "ezdbbackup ${version}" ]]; then
      echo "release binary reports unexpected version: ${version_output}" >&2
      exit 1
    fi
  fi

  install -m 0644 README.md config.example.yml "${package_dir}/"
  archive="${release_dist}/ezdbbackup_${version}_linux_${arch}.tar.gz"
  tar -C "${package_dir}" -czf "${archive}" \
    ezdbbackup README.md config.example.yml

  deb_root="${package_root}/deb_${arch}"
  install -d -m 0755 \
    "${deb_root}/DEBIAN" \
    "${deb_root}/usr/bin" \
    "${deb_root}/usr/share/doc/ezdbbackup/examples"
  install -m 0755 "${binary}" "${deb_root}/usr/bin/ezdbbackup"
  install -m 0644 README.md "${deb_root}/usr/share/doc/ezdbbackup/README.md"
  install -m 0644 config.example.yml "${deb_root}/usr/share/doc/ezdbbackup/examples/config.yml"
  install -m 0644 packaging/debian/copyright "${deb_root}/usr/share/doc/ezdbbackup/copyright"
  sed \
    -e "s/@VERSION@/${package_version}/g" \
    -e "s/@DATE@/${release_date}/g" \
    packaging/debian/changelog.template >"${deb_root}/usr/share/doc/ezdbbackup/changelog"
  chmod 0644 "${deb_root}/usr/share/doc/ezdbbackup/changelog"
  gzip -n -9 "${deb_root}/usr/share/doc/ezdbbackup/changelog"
  cat >"${deb_root}/DEBIAN/control" <<EOF
Package: ezdbbackup
Version: ${package_version}
Architecture: ${arch}
Maintainer: EZ Game Host Support <support@ezgamehost.com>
Section: admin
Priority: optional
Depends: ca-certificates
Recommends: default-mysql-client, cron
Homepage: https://github.com/ezgamehost/ezdbbackup
Description: Static MySQL backup and S3 upload tool
 ezdbbackup runs named MySQL dump jobs, stores structured local logs, and
 uploads backup objects to Amazon S3 or compatible endpoints.
EOF
  chmod 0644 "${deb_root}/DEBIAN/control"

  deb="${release_dist}/ezdbbackup_${package_version}_${arch}.deb"
  dpkg-deb --root-owner-group --build "${deb_root}" "${deb}"
  verify_deb_package "${deb}" "${arch}" "${binary}"
done

(
  cd "${release_dist}"
  sha256sum ezdbbackup_*.deb ezdbbackup_*.tar.gz > SHA256SUMS
  sha256sum -c SHA256SUMS
)
