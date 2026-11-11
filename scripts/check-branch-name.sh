#!/usr/bin/env bash
# Validates a branch name against the Conventional Branch rules in AGENTS.md.
# Used by CI (branch-name job) and the local pre-push hook, so the rule lives in one place.
#
# Usage: scripts/check-branch-name.sh <branch-name>
set -euo pipefail

branch="${1:?usage: $0 <branch-name>}"
pattern='^(feature|feat|bugfix|fix|hotfix|release|chore)/[a-z0-9]+(-[a-z0-9]+)*(\.[0-9]+)*$'

if [[ "$branch" =~ $pattern ]]; then
  echo "branch name ok: $branch"
  exit 0
fi

cat >&2 <<EOF
invalid branch name: $branch

Use Conventional Branch: <type>/<description>
  types: feature, feat, bugfix, fix, hotfix, release, chore
  lowercase a-z, 0-9 and single hyphens; dots only in release versions
  examples: feature/phase-0-foundations, fix/issue-12-rounding, release/v1.2.0
EOF
exit 1
