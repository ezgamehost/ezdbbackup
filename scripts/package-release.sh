#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${repository_root}"

version="${GITHUB_REF_NAME:-}"
if [[ ! "${version}" =~ ^v[0-9A-Za-z][0-9A-Za-z._-]*$ ]]; then
  echo "unsupported release tag for artifact names: ${version}" >&2
  exit 1
fi

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
done

(
  cd "${release_dist}"
  sha256sum ezdbbackup_*.tar.gz > SHA256SUMS
  sha256sum -c SHA256SUMS
)
