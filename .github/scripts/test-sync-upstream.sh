#!/usr/bin/env bash

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SCRIPT="$ROOT/.github/scripts/sync-upstream.sh"
VERSION_FILE=backend/cmd/server/VERSION
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

configure_git() {
  local repo=$1

  git -C "$repo" config user.name 'Sync Test'
  git -C "$repo" config user.email 'sync-test@example.com'
}

write_version() {
  local repo=$1
  local version=$2

  mkdir -p "$repo/backend/cmd/server"
  printf '%s\n' "$version" >"$repo/backend/cmd/server/VERSION"
}

commit_all() {
  local repo=$1
  local message=$2

  git -C "$repo" add .
  git -C "$repo" commit -q -m "$message"
}

create_fixture() {
  local name=$1
  local mode=$2
  local fixture="$TMP_ROOT/$name"
  local seed="$fixture/seed"
  local base_sha

  rm -rf "$fixture"
  mkdir -p "$fixture"
  git init -q --bare "$fixture/origin.git"
  git init -q --bare "$fixture/upstream.git"
  git init -q -b main "$seed"
  configure_git "$seed"

  write_version "$seed" 0.1.160
  mkdir -p "$seed/backend"
  printf 'base\n' >"$seed/backend/source.txt"
  commit_all "$seed" 'base release'
  base_sha=$(git -C "$seed" rev-parse HEAD)

  git -C "$seed" remote add origin "$fixture/origin.git"
  git -C "$seed" remote add upstream "$fixture/upstream.git"
  git -C "$seed" push -q origin main
  git -C "$seed" push -q upstream main

  case "$mode" in
    normal | rebase-version)
      git -C "$seed" switch -q -c feat/openai-403-config "$base_sha"
      printf 'custom patch\n' >"$seed/backend/custom.txt"
      commit_all "$seed" 'custom patch'
      if [[ $mode == rebase-version ]]; then
        write_version "$seed" 0.1.160-custom1
        commit_all "$seed" 'old custom version'
      fi
      git -C "$seed" push -q origin feat/openai-403-config

      git -C "$seed" switch -q -C upstream-main "$base_sha"
      write_version "$seed" 0.1.163
      printf 'upstream change\n' >"$seed/backend/upstream.txt"
      commit_all "$seed" 'upstream 0.1.163'
      git -C "$seed" push -q upstream HEAD:main
      ;;
    merge-version)
      git -C "$seed" switch -q -C origin-main "$base_sha"
      write_version "$seed" 0.1.160-custom1
      commit_all "$seed" 'origin custom version'
      git -C "$seed" push -q origin HEAD:main

      git -C "$seed" switch -q -c feat/openai-403-config
      printf 'custom patch\n' >"$seed/backend/custom.txt"
      commit_all "$seed" 'custom patch'
      git -C "$seed" push -q origin feat/openai-403-config

      git -C "$seed" switch -q -C upstream-main "$base_sha"
      write_version "$seed" 0.1.163
      printf 'upstream change\n' >"$seed/backend/upstream.txt"
      commit_all "$seed" 'upstream 0.1.163'
      git -C "$seed" push -q upstream HEAD:main
      ;;
    source-conflict)
      git -C "$seed" switch -q -C origin-main "$base_sha"
      printf 'origin change\n' >"$seed/backend/source.txt"
      commit_all "$seed" 'origin source change'
      git -C "$seed" push -q origin HEAD:main

      git -C "$seed" switch -q -c feat/openai-403-config
      printf 'custom patch\n' >"$seed/backend/custom.txt"
      commit_all "$seed" 'custom patch'
      git -C "$seed" push -q origin feat/openai-403-config

      git -C "$seed" switch -q -C upstream-main "$base_sha"
      printf 'upstream change\n' >"$seed/backend/source.txt"
      write_version "$seed" 0.1.163
      commit_all "$seed" 'upstream source change'
      git -C "$seed" push -q upstream HEAD:main
      ;;
    *)
      printf 'unknown fixture mode: %s\n' "$mode" >&2
      return 1
      ;;
  esac

  git --git-dir="$fixture/origin.git" symbolic-ref HEAD refs/heads/main
  git --git-dir="$fixture/upstream.git" symbolic-ref HEAD refs/heads/main
  git clone -q "$fixture/origin.git" "$fixture/runner"
  configure_git "$fixture/runner"
  git -C "$fixture/runner" remote add upstream "$fixture/upstream.git"
  git -C "$fixture/runner" fetch -q origin \
    '+refs/heads/main:refs/remotes/origin/main' \
    '+refs/heads/feat/openai-403-config:refs/remotes/origin/feat/openai-403-config'
  git -C "$fixture/runner" fetch -q upstream \
    '+refs/heads/main:refs/remotes/upstream/main'

  FIXTURE_RUNNER="$fixture/runner"
}

value_for() {
  local key=$1
  local file=$2

  sed -n "s/^${key}=//p" "$file" | tail -1
}

run_prepare() {
  local repo=$1
  shift

  (cd "$repo" && "$SCRIPT" prepare "$@")
}

run_publish() {
  local repo=$1
  shift

  (cd "$repo" && "$SCRIPT" publish "$@")
}

setup_publication_fixture() {
  local name=$1
  local output="$TMP_ROOT/$name-prepare-output"

  create_fixture "$name" normal
  run_prepare "$FIXTURE_RUNNER" 0.1.163 automation/upstream-sync-test >"$output"

  PUBLICATION_FIXTURE=$(dirname "$FIXTURE_RUNNER")
  PUBLICATION_ORIGIN_MAIN=$(value_for origin_main_sha "$output")
  PUBLICATION_ORIGIN_CUSTOM=$(value_for origin_custom_sha "$output")
  PUBLICATION_PREPARED_MAIN=$(value_for prepared_main_sha "$output")
  PUBLICATION_PREPARED_CUSTOM=$(value_for prepared_custom_sha "$output")
  PUBLICATION_CANDIDATE=$(value_for candidate_branch "$output")
  PUBLICATION_TAG=$(value_for target_tag "$output")
  PUBLICATION_MESSAGE="$PUBLICATION_FIXTURE/tag-message.md"
  printf 'Automated test release\n' >"$PUBLICATION_MESSAGE"

  git -C "$FIXTURE_RUNNER" push -q origin \
    "$PUBLICATION_PREPARED_CUSTOM:refs/heads/$PUBLICATION_CANDIDATE"
}

publish_fixture() {
  run_publish "$FIXTURE_RUNNER" \
    "$PUBLICATION_ORIGIN_MAIN" \
    "$PUBLICATION_ORIGIN_CUSTOM" \
    "$PUBLICATION_PREPARED_MAIN" \
    "$PUBLICATION_PREPARED_CUSTOM" \
    "$PUBLICATION_CANDIDATE" \
    "$PUBLICATION_TAG" \
    "$PUBLICATION_MESSAGE"
}

bare_ref() {
  local fixture=$1
  local ref=$2

  git --git-dir="$fixture/origin.git" rev-parse --verify "$ref"
}

assert_missing_bare_ref() {
  local fixture=$1
  local ref=$2

  if git --git-dir="$fixture/origin.git" rev-parse --verify --quiet "$ref" >/dev/null; then
    printf 'expected remote ref to be absent: %s\n' "$ref" >&2
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

test_normal_preparation() {
  local output
  local main_sha
  local custom_sha

  create_fixture normal normal
  output="$TMP_ROOT/normal-output"
  run_prepare "$FIXTURE_RUNNER" 0.1.163 automation/upstream-sync-test >"$output"

  main_sha=$(value_for prepared_main_sha "$output")
  custom_sha=$(value_for prepared_custom_sha "$output")
  assert_eq 0.1.163 "$(git -C "$FIXTURE_RUNNER" show "$main_sha:$VERSION_FILE")"
  assert_eq 0.1.163-custom1 "$(git -C "$FIXTURE_RUNNER" show "$custom_sha:$VERSION_FILE")"
  assert_eq 'custom patch' "$(git -C "$FIXTURE_RUNNER" show "$custom_sha:backend/custom.txt")"
  assert_eq 'upstream change' "$(git -C "$FIXTURE_RUNNER" show "$custom_sha:backend/upstream.txt")"
  assert_eq v0.1.163-custom1 "$(value_for target_tag "$output")"
}

test_merge_version_conflict() {
  local output
  local main_sha

  create_fixture merge-version merge-version
  output="$TMP_ROOT/merge-version-output"
  run_prepare "$FIXTURE_RUNNER" 0.1.163 automation/upstream-sync-test >"$output"

  main_sha=$(value_for prepared_main_sha "$output")
  assert_eq 0.1.163 "$(git -C "$FIXTURE_RUNNER" show "$main_sha:$VERSION_FILE")"
}

test_rebase_version_conflict() {
  local output
  local main_sha
  local custom_sha
  local rebased_subjects

  create_fixture rebase-version rebase-version
  output="$TMP_ROOT/rebase-version-output"
  run_prepare "$FIXTURE_RUNNER" 0.1.163 automation/upstream-sync-test >"$output"

  main_sha=$(value_for prepared_main_sha "$output")
  custom_sha=$(value_for prepared_custom_sha "$output")
  rebased_subjects=$(git -C "$FIXTURE_RUNNER" log --format=%s "$main_sha..$custom_sha")
  assert_eq 0.1.163-custom1 "$(git -C "$FIXTURE_RUNNER" show "$custom_sha:$VERSION_FILE")"
  if [[ $rebased_subjects == *'old custom version'* ]]; then
    printf 'empty VERSION-only commit was not skipped\n' >&2
    return 1
  fi
}

test_source_conflict_fails() {
  create_fixture source-conflict source-conflict
  assert_fails run_prepare "$FIXTURE_RUNNER" 0.1.163 automation/upstream-sync-test
}

test_atomic_publication() {
  local tag_target

  setup_publication_fixture publish-success
  publish_fixture

  assert_eq "$PUBLICATION_PREPARED_MAIN" "$(bare_ref "$PUBLICATION_FIXTURE" refs/heads/main)"
  assert_eq "$PUBLICATION_PREPARED_CUSTOM" "$(bare_ref "$PUBLICATION_FIXTURE" refs/heads/feat/openai-403-config)"
  assert_eq tag "$(git --git-dir="$PUBLICATION_FIXTURE/origin.git" cat-file -t "refs/tags/$PUBLICATION_TAG")"
  tag_target=$(git --git-dir="$PUBLICATION_FIXTURE/origin.git" rev-parse "refs/tags/$PUBLICATION_TAG^{}")
  assert_eq "$PUBLICATION_PREPARED_CUSTOM" "$tag_target"
}

test_main_lease_rejects_publication() {
  local concurrent="$TMP_ROOT/concurrent-main"
  local concurrent_sha

  setup_publication_fixture publish-main-race
  git clone -q --branch main "$PUBLICATION_FIXTURE/origin.git" "$concurrent"
  configure_git "$concurrent"
  printf 'concurrent update\n' >"$concurrent/concurrent.txt"
  commit_all "$concurrent" 'concurrent main update'
  git -C "$concurrent" push -q origin main
  concurrent_sha=$(git -C "$concurrent" rev-parse HEAD)

  assert_fails publish_fixture
  assert_eq "$concurrent_sha" "$(bare_ref "$PUBLICATION_FIXTURE" refs/heads/main)"
  assert_eq "$PUBLICATION_ORIGIN_CUSTOM" "$(bare_ref "$PUBLICATION_FIXTURE" refs/heads/feat/openai-403-config)"
  assert_missing_bare_ref "$PUBLICATION_FIXTURE" "refs/tags/$PUBLICATION_TAG"
}

test_candidate_mismatch_rejects_publication() {
  setup_publication_fixture publish-candidate-race
  git -C "$FIXTURE_RUNNER" push -q --force origin \
    "$PUBLICATION_ORIGIN_CUSTOM:refs/heads/$PUBLICATION_CANDIDATE"

  assert_fails publish_fixture
  assert_eq "$PUBLICATION_ORIGIN_MAIN" "$(bare_ref "$PUBLICATION_FIXTURE" refs/heads/main)"
  assert_eq "$PUBLICATION_ORIGIN_CUSTOM" "$(bare_ref "$PUBLICATION_FIXTURE" refs/heads/feat/openai-403-config)"
  assert_missing_bare_ref "$PUBLICATION_FIXTURE" "refs/tags/$PUBLICATION_TAG"
}

test_candidate_cleanup_scope() {
  local keep_branch=manual-keep

  setup_publication_fixture cleanup
  git -C "$FIXTURE_RUNNER" push -q origin \
    "$PUBLICATION_PREPARED_CUSTOM:refs/heads/automation/upstream-sync-stale" \
    "$PUBLICATION_PREPARED_CUSTOM:refs/heads/$keep_branch"

  (cd "$FIXTURE_RUNNER" && "$SCRIPT" cleanup-candidates)

  assert_missing_bare_ref "$PUBLICATION_FIXTURE" "refs/heads/$PUBLICATION_CANDIDATE"
  assert_missing_bare_ref "$PUBLICATION_FIXTURE" refs/heads/automation/upstream-sync-stale
  assert_eq "$PUBLICATION_PREPARED_CUSTOM" "$(bare_ref "$PUBLICATION_FIXTURE" "refs/heads/$keep_branch")"
}

test_workflow_entry_points() {
  rg -q '^  workflow_dispatch:' "$ROOT/.github/workflows/backend-ci.yml"
  rg -Fq 'run-name: Release ${{ github.event.inputs.tag || github.ref_name }}' \
    "$ROOT/.github/workflows/release.yml"
}

test_orchestration_contract() {
  local workflow="$ROOT/.github/workflows/sync-upstream-release.yml"

  [[ -f $workflow ]]
  rg -q "cron: '17 1 \* \* \*'" "$workflow"
  rg -q '^  workflow_dispatch:' "$workflow"
  rg -q 'cancel-in-progress: false' "$workflow"
  rg -q '^  actions: write' "$workflow"
  rg -q '^  contents: write' "$workflow"
  rg -q 'backend-ci\.yml' "$workflow"
  rg -q 'release\.yml' "$workflow"
  rg -q 'simple_release=true' "$workflow"
  rg -q 'gh run watch' "$workflow"
  rg -q 'gh release view' "$workflow"
  rg -q 'if: always\(\)' "$workflow"
  if rg -q '(^|[^A-Z_])PAT([^A-Z_]|$)' "$workflow"; then
    printf 'workflow must not depend on a PAT\n' >&2
    return 1
  fi
}

test_version_state
test_normal_preparation
test_merge_version_conflict
test_rebase_version_conflict
test_source_conflict_fails
test_atomic_publication
test_main_lease_rejects_publication
test_candidate_mismatch_rejects_publication
test_candidate_cleanup_scope
test_workflow_entry_points
test_orchestration_contract
printf 'all sync-upstream tests passed\n'
