#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${ROOT_DIR}"

GO_BIN="${GO_BIN:-go}"

git_version() {
  git describe --tags --always --dirty 2>/dev/null || printf 'dev'
}

git_commit() {
  git rev-parse --short=12 HEAD 2>/dev/null || printf 'unknown'
}

utc_build_date() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

VERSION="${VERSION:-$(git_version)}"
COMMIT="${COMMIT:-$(git_commit)}"
BUILD_DATE="${BUILD_DATE:-$(utc_build_date)}"

if [[ "${1:-}" == "shellvars" ]]; then
  cat <<EOF
VERSION='${VERSION}'
COMMIT='${COMMIT}'
BUILD_DATE='${BUILD_DATE}'
EOF
  exit 0
fi

strip_bin=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --strip)
      strip_bin=1
      shift
      ;;
    --)
      shift
      break
      ;;
    *)
      break
      ;;
  esac
done

ldflags=(
  "-X" "main.version=${VERSION}"
  "-X" "main.commit=${COMMIT}"
  "-X" "main.buildDate=${BUILD_DATE}"
)

if [[ -n "${EXTRA_LDFLAGS:-}" ]]; then
  # Intentionally word-split to allow callers to pass multiple ldflags fragments.
  # shellcheck disable=SC2206
  extra_ldflags=(${EXTRA_LDFLAGS})
  ldflags+=("${extra_ldflags[@]}")
fi
if [[ "${strip_bin}" -eq 1 ]]; then
  ldflags+=("-s" "-w")
fi

has_pkg=0
args=("$@")
i=0
while [[ ${i} -lt ${#args[@]} ]]; do
  arg="${args[$i]}"
  if [[ "${arg}" == "--" ]]; then
    i=$((i + 1))
    break
  fi
  case "${arg}" in
    -a|-asan|-buildvcs|-msan|-n|-race|-trimpath|-v|-work|-x)
      ;;
    -C|-buildmode|-compiler|-cover|-covermode|-coverpkg|-gcflags|-asmflags|-installsuffix|-ldflags|-mod|-modfile|-overlay|-o|-p|-pkgdir|-tags|-toolexec)
      i=$((i + 1))
      ;;
    -C=*|-buildmode=*|-compiler=*|-cover=*|-covermode=*|-coverpkg=*|-gcflags=*|-asmflags=*|-installsuffix=*|-ldflags=*|-mod=*|-modfile=*|-overlay=*|-o=*|-p=*|-pkgdir=*|-tags=*|-toolexec=*)
      ;;
    -*)
      # Unknown flag; assume caller knows what they're doing and continue.
      ;;
    *)
      has_pkg=1
      break
      ;;
  esac
  i=$((i + 1))
done
if [[ "${has_pkg}" -eq 0 && ${i} -lt ${#args[@]} ]]; then
  has_pkg=1
fi
if [[ "${has_pkg}" -eq 0 ]]; then
  set -- "$@" "${DEFAULT_BUILD_PKG:-./cmd/contactd}"
fi

export CGO_ENABLED="${CGO_ENABLED:-0}"

exec "${GO_BIN}" build -trimpath -ldflags "${ldflags[*]}" "$@"
