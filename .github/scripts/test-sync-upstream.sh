#!/usr/bin/env bash

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SCRIPT="$ROOT/.github/scripts/sync-upstream.sh"
TMP_ROOT=$(mktemp -d)
trap 'rm -rf "$TMP_ROOT"' EXIT

assert_eq() {
  local expected=$1
  local actual=$2

  if [[ "$expected" != "$actual" ]]; then
    printf 'expected <%s>, got <%s>\n' "$expected" "$actual" >&2
    return 1
  fi
}

assert_fails() {
  if "$@" >/dev/null 2>&1; then
    printf 'expected command to fail:' >&2
    printf ' %q' "$@" >&2
    printf '\n' >&2
    return 1
  fi
}

test_version_state() {
  local latest

  latest=$(printf '%s\n' \
    v0.1.160-custom1 \
    unrelated \
    v0.1.9-custom1 \
    v0.1.163-custom1 | "$SCRIPT" latest-release)

  assert_eq 0.1.163 "$latest"
  assert_eq noop "$("$SCRIPT" classify 0.1.163 0.1.163 true true)"
  assert_eq retry-release "$("$SCRIPT" classify 0.1.163 0.1.160 true false)"
  assert_eq prepare "$("$SCRIPT" classify 0.1.163 0.1.160 false false)"
  assert_eq prepare "$("$SCRIPT" classify 0.1.163 '' false false)"
  assert_fails "$SCRIPT" classify 0.1.159 0.1.160 false false
  assert_fails "$SCRIPT" classify invalid 0.1.160 false false
  assert_fails "$SCRIPT" classify 0.1.163 invalid false false
  assert_fails "$SCRIPT" classify 0.1.163 0.1.160 maybe false
}

test_version_state
printf 'version state tests passed\n'
