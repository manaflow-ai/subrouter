#!/usr/bin/env bash
# Runs the "Classify recheck authorization" step body straight out of
# cla.yml against the outcomes GitHub can hand it.
#
# The classifier treated a skipped authorization step as a failure. On a
# pull_request_target event there is no recheck command, so that step is always
# skipped, and every ordinary pull request failed the trigger job and therefore
# the required CLA check. The step is fail-closed by never granting
# authorization, which is a separate property from failing the job.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW="$HERE/../.github/workflows/cla.yml"
failures=0

check() {
  if [ "$2" -eq 0 ]; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s\n' "$1"; failures=$((failures + 1)); fi
}

# Extract the classifier body: the `run: |` block of the step whose id is
# classify-recheck, dedented to a runnable script.
extract_classifier() {
  # Deliberately no PyYAML: this test must run on any runner that has a stock
  # python3. The block is found by its step id and taken by indentation.
  python3 - "$WORKFLOW" <<'PY'
import sys

lines = open(sys.argv[1], encoding="utf-8").read().splitlines()
start = None
for index, line in enumerate(lines):
    if line.strip() == "id: classify-recheck":
        for cursor in range(index, len(lines)):
            if lines[cursor].strip() in ("run: |", "run: |-"):
                start = cursor + 1
                break
        break
if start is None:
    raise SystemExit("classify-recheck run block not found")

indent = len(lines[start]) - len(lines[start].lstrip())
body = []
for line in lines[start:]:
    if line.strip() and (len(line) - len(line.lstrip())) < indent:
        break
    body.append(line[indent:] if len(line) >= indent else line)
sys.stdout.write("\n".join(body).rstrip() + "\n")
PY
}

run_classifier() { # run_classifier <outcome> <authorized> <decision>
  local script output_file
  script="$(mktemp)"; output_file="$(mktemp)"
  extract_classifier >"$script" || return 99
  ACTION_OUTCOME="$1" ACTION_AUTHORIZED="$2" ACTION_DECISION="$3" \
    GITHUB_OUTPUT="$output_file" bash "$script" >/dev/null 2>&1
  local status=$?
  CLASSIFIER_OUTPUT="$(cat "$output_file")"
  rm -f "$script" "$output_file"
  return $status
}

run_classifier skipped false ""
status=$?
[ "$status" -eq 0 ]
check "a skipped authorization step does not fail the job" $?
grep -q "authorized=false" <<<"$CLASSIFIER_OUTPUT"
check "a skipped authorization step grants nothing" $?

run_classifier success true authorized
status=$?
[ "$status" -eq 0 ] && grep -q "authorized=true" <<<"$CLASSIFIER_OUTPUT"
check "an authorized recheck is admitted" $?

run_classifier success false unauthorized
status=$?
[ "$status" -eq 0 ] && grep -q "authorized=false" <<<"$CLASSIFIER_OUTPUT"
check "an unauthorized recheck is denied without failing" $?

run_classifier success true unauthorized
[ $? -eq 0 ] && grep -q "decision=error" <<<"$CLASSIFIER_OUTPUT"
check "an inconsistent denial is reported as an error" $?

run_classifier failure false ""
[ $? -ne 0 ]
check "a failed authorization step still fails the job" $?

run_classifier cancelled false ""
[ $? -ne 0 ]
check "a cancelled authorization step still fails the job" $?

if [ "$failures" -ne 0 ]; then printf '%d check(s) failed\n' "$failures"; exit 1; fi
printf 'CLA recheck classifier tests passed\n'
