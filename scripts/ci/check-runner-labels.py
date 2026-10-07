#!/usr/bin/env python3
"""Refuse GitHub-hosted runner labels in GitHub Actions workflows.

A GitHub billing block or hosted outage must never stop CI, so jobs run on
Blacksmith labels (blacksmith-*) or owned runners, never on GitHub-hosted
images. The check reads workflow text line by line (no YAML dependency), so it
also sees labels in matrix rows, expressions, dispatch defaults and *RUNNER*
environment mirrors.

Allowed:
  - comments, `name:` values and `run:` script text (not runner selection);
  - the fork branch `github.repository_owner != '<owner>' && '<label>'`, which
    never evaluates in the owner's repository;
  - a line ending in `# runner-policy-exception: <reason>`, for a job that
    cannot run off GitHub-hosted (for example npm provenance publishing).

Usage: check-runner-labels.py [workflow files or directories]
(default: .github/workflows). Exits 1 and prints file:line for each violation.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

HOSTED = re.compile(
    r"(?<![A-Za-z0-9_-])("
    r"ubuntu-(?:latest|slim|\d{2}\.\d{2}(?:-arm)?)"
    r"|macos-(?:latest|\d+(?:-intel|-large|-xlarge|-arm64)?)"
    r"|windows-(?:latest|\d{4}(?:-arm)?|11-arm)"
    r")(?![A-Za-z0-9_.-])"
)
FORK_BRANCH = re.compile(r"github\.repository_owner\s*!=\s*'[^']+'\s*&&\s*'[^']+'")
EXCEPTION = re.compile(r"#\s*runner-policy-exception:\s*\S")
# Lines whose value is prose or shell, not a runner label.
NON_RUNNER_KEY = re.compile(r"^\s*(?:-\s+)?(?:name|description|run|echo)\s*:")


def _strip_comment(line: str) -> str:
    # A `#` starts a YAML comment only at the line start or after whitespace,
    # and not inside an expression or quoted string; workflow lines that hold
    # a runner label do not put `#` inside quotes, so this is enough here.
    match = re.search(r"(^|\s)#", line)
    return line[: match.start()] if match else line


def violations(paths: list[Path]) -> list[str]:
    found: list[str] = []
    for path in paths:
        in_block_scalar = False
        block_indent = 0
        for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
            indent = len(line) - len(line.lstrip())
            if in_block_scalar:
                if line.strip() and indent <= block_indent:
                    in_block_scalar = False
                else:
                    continue
            if re.search(r":\s*[|>][-+0-9]*\s*(#.*)?$", line) and NON_RUNNER_KEY.match(line):
                in_block_scalar, block_indent = True, indent
                continue
            if NON_RUNNER_KEY.match(line):
                continue
            code = FORK_BRANCH.sub("", _strip_comment(line))
            if not HOSTED.search(code):
                continue
            if EXCEPTION.search(line):
                continue
            found.append(f"{path}:{number}: {line.strip()}")
    return found


def _workflow_files(args: list[str]) -> list[Path]:
    targets = [Path(a) for a in args] or [Path(".github/workflows")]
    files: list[Path] = []
    for target in targets:
        if target.is_dir():
            files += sorted(p for p in target.iterdir() if p.suffix in (".yml", ".yaml"))
        else:
            files.append(target)
    return files


def main(argv: list[str]) -> int:
    found = violations(_workflow_files(argv))
    if found:
        print("GitHub-hosted runner labels are not allowed (a billing block must not stop CI):")
        print("\n".join(found))
        print("Use a blacksmith-* label or an owned runner. If a job cannot move, end the line with")
        print("`# runner-policy-exception: <reason>`.")
        return 1
    print("PASS: no workflow selects a GitHub-hosted runner")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
