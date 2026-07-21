#!/usr/bin/env bash

set -euo pipefail

VERSION_FILE=backend/cmd/server/VERSION
MAIN_BRANCH=main
CUSTOM_BRANCH=feat/openai-403-config
CUSTOM_SUFFIX=custom1
CANDIDATE_PREFIX=automation/upstream-sync-
PREPARED_MAIN_BRANCH=automation-prepared-main
PREPARED_CUSTOM_BRANCH=automation-prepared-custom

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

only_version_conflicted() {
  local conflicts

  conflicts=$(git diff --name-only --diff-filter=U)
  [[ $conflicts == "$VERSION_FILE" ]]
}

merge_upstream() {
  local merge_status

  set +e
  GIT_MERGE_AUTOEDIT=no git merge --no-edit "upstream/$MAIN_BRANCH" >&2
  merge_status=$?
  set -e

  if (( merge_status == 0 )); then
    return
  fi

  if ! only_version_conflicted; then
    git merge --abort >/dev/null 2>&1 || true
    die 'upstream merge has conflicts outside the VERSION file'
  fi

  git show "upstream/$MAIN_BRANCH:$VERSION_FILE" >"$VERSION_FILE"
  git add "$VERSION_FILE"
  GIT_EDITOR=true git commit --no-edit >&2
}

continue_rebase() {
  local rebase_status

  set +e
  GIT_EDITOR=true git rebase "$PREPARED_MAIN_BRANCH" >&2
  rebase_status=$?
  set -e

  while (( rebase_status != 0 )); do
    if ! git rev-parse --verify --quiet REBASE_HEAD >/dev/null; then
      git rebase --abort >/dev/null 2>&1 || true
      die 'custom branch rebase failed without a resolvable conflict'
    fi

    if ! only_version_conflicted; then
      git rebase --abort >/dev/null 2>&1 || true
      die 'custom branch rebase has conflicts outside the VERSION file'
    fi

    git show "$PREPARED_MAIN_BRANCH:$VERSION_FILE" >"$VERSION_FILE"
    git add "$VERSION_FILE"

    set +e
    if git diff --cached --quiet; then
      GIT_EDITOR=true git rebase --skip >&2
    else
      GIT_EDITOR=true git rebase --continue >&2
    fi
    rebase_status=$?
    set -e
  done
}

prepare() {
  local upstream_version=$1
  local candidate_branch=$2
  local origin_main_sha
  local origin_custom_sha
  local prepared_main_sha
  local prepared_custom_sha
  local target_version

  validate_version "$upstream_version"
  [[ $candidate_branch == "$CANDIDATE_PREFIX"* ]] || die "invalid candidate branch: $candidate_branch"
  git check-ref-format --branch "$candidate_branch" >/dev/null || die "invalid candidate branch: $candidate_branch"

  origin_main_sha=$(git rev-parse --verify "origin/$MAIN_BRANCH")
  origin_custom_sha=$(git rev-parse --verify "origin/$CUSTOM_BRANCH")
  git rev-parse --verify "upstream/$MAIN_BRANCH" >/dev/null

  git switch -C "$PREPARED_MAIN_BRANCH" "$origin_main_sha" >&2
  merge_upstream
  prepared_main_sha=$(git rev-parse HEAD)

  git switch -C "$PREPARED_CUSTOM_BRANCH" "$origin_custom_sha" >&2
  continue_rebase

  target_version="${upstream_version}-${CUSTOM_SUFFIX}"
  printf '%s\n' "$target_version" >"$VERSION_FILE"
  git add "$VERSION_FILE"
  if ! git diff --cached --quiet; then
    git commit -m "chore: sync VERSION to $target_version" >&2
  fi
  prepared_custom_sha=$(git rev-parse HEAD)

  printf 'origin_main_sha=%s\n' "$origin_main_sha"
  printf 'origin_custom_sha=%s\n' "$origin_custom_sha"
  printf 'prepared_main_sha=%s\n' "$prepared_main_sha"
  printf 'prepared_custom_sha=%s\n' "$prepared_custom_sha"
  printf 'candidate_branch=%s\n' "$candidate_branch"
  printf 'target_version=%s\n' "$target_version"
  printf 'target_tag=v%s\n' "$target_version"
}

validate_sha() {
  [[ ${1:-} =~ ^[0-9a-f]{40,64}$ ]] || die "invalid Git SHA: ${1:-<empty>}"
}

remote_head_sha() {
  local branch=$1

  git ls-remote --heads origin "refs/heads/$branch" | awk 'NR == 1 { print $1 }'
}

remote_tag_exists() {
  local tag=$1

  [[ -n $(git ls-remote --tags origin "refs/tags/$tag") ]]
}

publish() {
  local expected_main=$1
  local expected_custom=$2
  local prepared_main=$3
  local prepared_custom=$4
  local candidate_branch=$5
  local target_tag=$6
  local tag_message_file=$7
  local actual_main
  local actual_custom
  local actual_candidate

  validate_sha "$expected_main"
  validate_sha "$expected_custom"
  validate_sha "$prepared_main"
  validate_sha "$prepared_custom"
  [[ $candidate_branch == "$CANDIDATE_PREFIX"* ]] || die "invalid candidate branch: $candidate_branch"
  git check-ref-format --branch "$candidate_branch" >/dev/null || die "invalid candidate branch: $candidate_branch"
  [[ $target_tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+-custom1$ ]] || die "invalid target tag: $target_tag"
  [[ -f $tag_message_file ]] || die "tag message file does not exist: $tag_message_file"
  git cat-file -e "$prepared_main^{commit}"
  git cat-file -e "$prepared_custom^{commit}"

  actual_main=$(remote_head_sha "$MAIN_BRANCH")
  actual_custom=$(remote_head_sha "$CUSTOM_BRANCH")
  actual_candidate=$(remote_head_sha "$candidate_branch")

  [[ $actual_main == "$expected_main" ]] || die 'origin main changed after candidate preparation'
  [[ $actual_custom == "$expected_custom" ]] || die 'origin custom branch changed after candidate preparation'
  [[ $actual_candidate == "$prepared_custom" ]] || die 'candidate ref does not match the tested custom SHA'
  ! remote_tag_exists "$target_tag" || die "target tag already exists: $target_tag"
  ! git show-ref --verify --quiet "refs/tags/$target_tag" || die "local target tag already exists: $target_tag"

  git tag -a "$target_tag" "$prepared_custom" -F "$tag_message_file"
  git push --atomic \
    --force-with-lease="refs/heads/${CUSTOM_BRANCH}:${expected_custom}" \
    origin \
    "${prepared_main}:refs/heads/${MAIN_BRANCH}" \
    "${prepared_custom}:refs/heads/${CUSTOM_BRANCH}" \
    "refs/tags/${target_tag}:refs/tags/${target_tag}" >&2
}

cleanup_candidates() {
  local ref
  local branch
  local status=0

  while read -r _ ref; do
    [[ -n ${ref:-} ]] || continue
    branch=${ref#refs/heads/}
    git push origin --delete "$branch" >&2 || status=1
  done < <(git ls-remote --heads origin "refs/heads/${CANDIDATE_PREFIX}*")

  return "$status"
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
  prepare)
    [[ $# == 2 ]] || die 'usage: prepare UPSTREAM_VERSION CANDIDATE_BRANCH'
    prepare "$@"
    ;;
  publish)
    [[ $# == 7 ]] || die 'usage: publish EXPECTED_MAIN EXPECTED_CUSTOM PREPARED_MAIN PREPARED_CUSTOM CANDIDATE_BRANCH TARGET_TAG TAG_MESSAGE_FILE'
    publish "$@"
    ;;
  cleanup-candidates)
    [[ $# == 0 ]] || die 'cleanup-candidates takes no arguments'
    cleanup_candidates
    ;;
  *)
    die "unknown command: ${command:-<empty>}"
    ;;
esac
