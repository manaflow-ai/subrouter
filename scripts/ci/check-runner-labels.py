#!/usr/bin/env python3
"""Refuse GitHub-hosted runner labels in GitHub Actions workflows.

The check parses workflow YAML so matrix values referenced by runs-on cannot
hide a hosted label. It fails closed when the parser is unavailable or the
workflow cannot be parsed.

Allowed:
  - comments, name values, and run script text (not runner selection);
  - the fork branch github.repository_owner != 'manaflow-ai' && '<label>',
    which never evaluates in the owner's repository;
  - a line ending in # runner-policy-exception: <reason> for a job that
    cannot run off GitHub-hosted (for example trusted publishing).

Usage: check-runner-labels.py [workflow files or directories]
(default: .github/workflows). Exits 1 and prints file:line for each violation.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

try:
    import yaml
except ImportError as exc:  # pragma: no cover - exercised by the CI guard.
    yaml = None
    _YAML_IMPORT_ERROR = exc


HOSTED = re.compile(
    r"(?<![A-Za-z0-9_-])("
    r"ubuntu-(?:latest|slim|\d{2}\.\d{2}(?:-\d+core)?(?:-arm)?)"
    r"|macos-(?:latest|\d+(?:-intel|-large|-xlarge|-arm64)?)"
    r"|windows-(?:latest|\d{4}(?:-arm)?|11(?:-vs\d+)?-arm)"
    r"|xcode-\d+(?:\.\d+)?"
    r")(?![A-Za-z0-9_.-])"
)
FORK_BRANCH = re.compile(
    r"github\.repository_owner\s*!=\s*['\"]manaflow-ai['\"]\s*&&\s*"
    r"['\"](?P<label>[^'\"]+)['\"]"
)
EXCEPTION = re.compile(r"#\s*runner-policy-exception:\s*\S")


def _mapping_value(node, key):
    """Return a composed YAML mapping value while retaining its line mark."""
    if node is None or not hasattr(node, "value"):
        return None
    for key_node, value_node in node.value:
        if key_node.value == key:
            return value_node
    return None


def _strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for child in value.values():
            yield from _strings(child)
    elif isinstance(value, (list, tuple)):
        for child in value:
            yield from _strings(child)


def _hosted_labels(value):
    return [match.group(1) for match in HOSTED.finditer(value)]


def _fork_only_labels(value):
    """Return hosted labels confined to the non-owner fork branch."""
    allowed = set()
    hosted = list(HOSTED.finditer(value))
    for match in FORK_BRANCH.finditer(value):
        label_start = match.start("label")
        label_end = match.end("label")
        branch_labels = [
            item.group(1)
            for item in hosted
            if item.start(1) >= label_start and item.end(1) <= label_end
        ]
        if len(branch_labels) == 1 and branch_labels[0] == match.group("label") and len(hosted) == 1:
            allowed.add(branch_labels[0])
    return allowed


def _node_has_exception(node, lines):
    if node is None or node.start_mark.line >= len(lines):
        return False
    # Keep exceptions attached to the actual runs-on declaration. A comment
    # on an unrelated matrix or step must not exempt the selector.
    return bool(EXCEPTION.search(lines[node.start_mark.line]))


def _job_nodes(root):
    jobs = _mapping_value(root, "jobs")
    if jobs is None or not hasattr(jobs, "value"):
        return []
    return [(key_node.value, value_node) for key_node, value_node in jobs.value]


def violations(paths: list[Path]) -> list[str]:
    found: list[str] = []
    for path in paths:
        text = path.read_text(encoding="utf-8")
        lines = text.splitlines()
        if yaml is None:
            found.append(f"{path}: YAML parser unavailable: {_YAML_IMPORT_ERROR}")
            continue
        try:
            document = yaml.safe_load(text) or {}
            root = yaml.compose(text)
        except yaml.YAMLError as exc:
            found.append(f"{path}: YAML parse failed: {exc}")
            continue
        jobs = document.get("jobs") if isinstance(document, dict) else None
        if not isinstance(jobs, dict):
            found.append(f"{path}: workflow has no parseable jobs mapping")
            continue
        node_by_job = dict(_job_nodes(root))
        for job_name, job in jobs.items():
            if not isinstance(job, dict):
                continue
            runs_on = job.get("runs-on")
            if runs_on is None:
                continue
            runs_node = _mapping_value(node_by_job.get(job_name), "runs-on")
            line = runs_node.start_mark.line + 1 if runs_node else 1
            selector_values = list(_strings(runs_on))
            matrix_ref = any("matrix." in value or "matrix[" in value for value in selector_values)
            strategy = job.get("strategy")
            matrix = strategy.get("matrix") if isinstance(strategy, dict) else None
            if matrix_ref:
                if matrix is None or not isinstance(matrix, (dict, list)):
                    found.append(f"{path}:{line}: {job_name}: dynamic matrix runner cannot be verified")
                    continue
                selector_values.extend(_strings(matrix))
                if any("${{" in value for value in _strings(matrix)):
                    found.append(f"{path}:{line}: {job_name}: dynamic matrix runner cannot be verified")
                    continue
            for selector in selector_values:
                hosted = _hosted_labels(selector)
                if not hosted:
                    if "${{" in selector and not matrix_ref and not _fork_only_labels(selector):
                        found.append(f"{path}:{line}: {job_name}: dynamic runner cannot be verified")
                    continue
                allowed_fork = _fork_only_labels(selector)
                disallowed = [label for label in hosted if label not in allowed_fork]
                if disallowed and not _node_has_exception(runs_node, lines):
                    found.append(f"{path}:{line}: {job_name}: hosted runner label(s): {', '.join(disallowed)}")
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
        print("# runner-policy-exception: <reason>. Dynamic selectors must be statically verifiable.")
        return 1
    print("PASS: no workflow selects a GitHub-hosted runner")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
