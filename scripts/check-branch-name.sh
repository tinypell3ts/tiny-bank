#!/usr/bin/env bash
# Validates a branch name against the Conventional Branch rules in AGENTS.md.
# Used by CI (branch-name job) and the local pre-push hook, so the rule lives in one place.
# Tested by scripts/check-branch-name_test.sh.
#
# Usage: scripts/check-branch-name.sh <branch-name>
set -euo pipefail

branch="${1:?usage: $0 <branch-name>}"

# A description is lowercase words joined by single hyphens: issue-12-ledger-postings
slug='[a-z0-9]+(-[a-z0-9]+)*'
# Ordinary branches: no dots anywhere.
work_pattern="^(feature|feat|bugfix|fix|hotfix|chore)/${slug}\$"
# Release branches: dots only as a version, e.g. release/v1.2.0
release_pattern='^release/v?[0-9]+(\.[0-9]+)*$'

if [[ "$branch" =~ $work_pattern || "$branch" =~ $release_pattern ]]; then
  echo "branch name ok: $branch"
  exit 0
fi

cat >&2 <<EOF
invalid branch name: $branch

Use Conventional Branch: <type>/<description>
  types: feature, feat, bugfix, fix, hotfix, chore, release
  lowercase a-z, 0-9 and single hyphens
  release branches are a version: release/v1.2.0
  examples: feature/phase-0-foundations, fix/issue-12-rounding, release/v1.2.0
EOF
exit 1
