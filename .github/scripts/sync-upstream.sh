#!/usr/bin/env bash

set -euo pipefail

VERSION_FILE=backend/cmd/server/VERSION
MAIN_BRANCH=main
CUSTOM_BRANCH=feat/openai-403-config
CUSTOM_SUFFIX=custom1
CANDIDATE_PREFIX=automation/upstream-sync-

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

validate_version() {
  [[ ${1:-} =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "invalid version: ${1:-<empty>}"
}

validate_bool() {
  [[ ${1:-} == true || ${1:-} == false ]] || die "invalid boolean: ${1:-<empty>}"
}

compare_versions() {
  local left=$1
  local right=$2
  local IFS=.
  local left_parts=()
  local right_parts=()
  local index

  validate_version "$left"
  validate_version "$right"
  read -r -a left_parts <<<"$left"
  read -r -a right_parts <<<"$right"

  for index in 0 1 2; do
    if (( 10#${left_parts[$index]} < 10#${right_parts[$index]} )); then
      printf '%s\n' -1
      return
    fi
    if (( 10#${left_parts[$index]} > 10#${right_parts[$index]} )); then
      printf '%s\n' 1
      return
    fi
  done

  printf '%s\n' 0
}

latest_release() {
  local tag
  local version
  local latest=''

  while IFS= read -r tag; do
    if [[ $tag =~ ^v([0-9]+\.[0-9]+\.[0-9]+)-custom1$ ]]; then
      version=${BASH_REMATCH[1]}
      if [[ -z $latest ]] || [[ $(compare_versions "$version" "$latest") == 1 ]]; then
        latest=$version
      fi
    fi
  done

  printf '%s\n' "$latest"
}

classify() {
  local upstream=${1:-}
  local latest=${2:-}
  local tag_exists=${3:-}
  local release_exists=${4:-}
  local comparison=1

  validate_version "$upstream"
  validate_bool "$tag_exists"
  validate_bool "$release_exists"

  if [[ -n $latest ]]; then
    validate_version "$latest"
    comparison=$(compare_versions "$upstream" "$latest")
    (( comparison >= 0 )) || die "upstream version $upstream is older than released version $latest"
  fi

  if [[ $release_exists == true ]] || [[ -n $latest && $comparison == 0 ]]; then
    printf 'noop\n'
  elif [[ $tag_exists == true ]]; then
    printf 'retry-release\n'
  else
    printf 'prepare\n'
  fi
}

command=${1:-}
shift || true

case "$command" in
  latest-release)
    [[ $# == 0 ]] || die 'latest-release takes no arguments'
    latest_release
    ;;
  classify)
    [[ $# == 4 ]] || die 'usage: classify UPSTREAM LATEST TAG_EXISTS RELEASE_EXISTS'
    classify "$@"
    ;;
  *)
    die "unknown command: ${command:-<empty>}"
    ;;
esac
