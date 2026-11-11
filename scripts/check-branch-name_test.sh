#!/usr/bin/env bash
# Tests for check-branch-name.sh. Run with: make test-scripts
set -euo pipefail

check="$(dirname "$0")/check-branch-name.sh"
failures=0

valid=(
  feature/phase-0-foundations
  feat/x
  bugfix/rounding
  fix/issue-12-rounding
  hotfix/ledger-lock
  chore/bump-go
  release/v1.2.0
  release/1.2
  release/v2
)

invalid=(
  main                    # trunk is not a work branch
  claude/zen-cori-3m52ui  # vendor prefix
  feature/Bad             # uppercase
  feature/a--b            # double hyphen
  feature/-a              # leading hyphen
  feature/a-              # trailing hyphen
  feature/foo.1           # dots only allowed in release versions
  fix/issue-12.3          # dots only allowed in release versions
  feature/                # empty description
  feature/a/b             # nested path
  release/next            # release must be a version
  release/v1..2           # double dot
  release/v1.2.           # trailing dot
  docs/readme             # unknown type
)

for name in "${valid[@]}"; do
  if ! "$check" "$name" >/dev/null 2>&1; then
    echo "FAIL: expected valid:   $name"
    failures=$((failures + 1))
  fi
done

for name in "${invalid[@]}"; do
  if "$check" "$name" >/dev/null 2>&1; then
    echo "FAIL: expected invalid: $name"
    failures=$((failures + 1))
  fi
done

total=$(( ${#valid[@]} + ${#invalid[@]} ))
if (( failures > 0 )); then
  echo "$failures of $total branch-name cases failed"
  exit 1
fi
echo "ok: $total branch-name cases"
