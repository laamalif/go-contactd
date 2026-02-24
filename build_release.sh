#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${ROOT_DIR}"

DIST_DIR="${DIST_DIR:-dist}"
BIN_NAME="${BIN_NAME:-go-contactd}"
PKG="${PKG:-./cmd/contactd}"

DEFAULT_TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "freebsd/amd64"
  "freebsd/arm64"
  "openbsd/amd64"
  "openbsd/arm64"
)

if [[ $# -gt 0 ]]; then
  TARGETS=("$@")
else
  TARGETS=("${DEFAULT_TARGETS[@]}")
fi

eval "$("${ROOT_DIR}/build_dist.sh" shellvars)"

safe_version="$(printf '%s' "${VERSION}" | tr -c 'A-Za-z0-9._-' '_')"
mkdir -p "${DIST_DIR}"

sha_cmd=""
if command -v sha256sum >/dev/null 2>&1; then
  sha_cmd="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  sha_cmd="shasum -a 256"
elif command -v sha256 >/dev/null 2>&1; then
  sha_cmd="sha256"
else
  printf 'error: need sha256sum, shasum, or sha256 in PATH\n' >&2
  exit 1
fi

checksum_line() {
  local file="$1"
  case "${sha_cmd}" in
    "sha256sum")
      sha256sum "${file}"
      ;;
    "shasum -a 256")
      shasum -a 256 "${file}"
      ;;
    "sha256")
      printf '%s  %s\n' "$(sha256 -q "${file}")" "${file}"
      ;;
  esac
}

checksum_file="${DIST_DIR}/SHA256SUMS"
: >"${checksum_file}"

for target in "${TARGETS[@]}"; do
  case "${target}" in
    */*)
      goos="${target%/*}"
      goarch="${target#*/}"
      ;;
    *)
      printf 'error: target must be GOOS/GOARCH, got %q\n' "${target}" >&2
      exit 1
      ;;
  esac

  out="${DIST_DIR}/${BIN_NAME}_${safe_version}_${goos}_${goarch}"
  printf '[release] building %s -> %s\n' "${target}" "${out}"
  VERSION="${VERSION}" \
  COMMIT="${COMMIT}" \
  BUILD_DATE="${BUILD_DATE}" \
  GOOS="${goos}" \
  GOARCH="${goarch}" \
  CGO_ENABLED=0 \
  "${ROOT_DIR}/build_dist.sh" --strip -o "${out}" "${PKG}"

  checksum_line "${out}" >>"${checksum_file}"
done

printf '[release] wrote %s\n' "${checksum_file}"
