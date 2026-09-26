#!/usr/bin/env python3
"""Safely retain a warm canary backend while switching one URL-map route."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
from typing import NoReturn


MAX_URL_MAP_BYTES = 8 * 1024 * 1024
ROOT_MATCHER = "__root__"
TOP_LEVEL_KEY = re.compile(r"^([A-Za-z][A-Za-z0-9_-]*):(?:[ \t]*(.*))?$")
FIRST_ITEM_FIELD = re.compile(r"^- ([A-Za-z][A-Za-z0-9_-]*):(?:[ \t]*(.*))?$")
ITEM_FIELD = re.compile(r"^  ([A-Za-z][A-Za-z0-9_-]*):(?:[ \t]*(.*))?$")
HOST_VALUE = re.compile(r"^  - ([^\s#][^\r\n]*)$")
SCALAR_FIELD_VALUE = re.compile(r"^\s*(?:-\s*)?[A-Za-z][A-Za-z0-9_-]*:\s*([^\s#]+)\s*$")
NAME = re.compile(r"^[a-z][a-z0-9-]{0,62}$")
HOST = re.compile(r"^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$")


class RoutingError(Exception):
    """The URL map does not satisfy the migration routing contract."""


def fail(message: str) -> NoReturn:
    raise RoutingError(message)


def read_map(path: Path) -> str:
    try:
        metadata = path.stat()
        if not stat.S_ISREG(metadata.st_mode) or metadata.st_size <= 0:
            fail("URL map must be a non-empty regular file")
        if metadata.st_size > MAX_URL_MAP_BYTES:
            fail("URL map exceeds the size limit")
        body = path.read_text(encoding="utf-8")
    except (OSError, UnicodeError) as error:
        fail(f"could not read URL map {path}: {error}")
    if "\r" in body or "\x00" in body:
        fail("URL map contains unsupported control characters")
    return body


def write_map(path: Path, body: str, source: Path) -> None:
    try:
        if path.is_symlink():
            fail("candidate URL-map destination must not be a symlink")
        if source.resolve() == path.resolve():
            fail("candidate URL-map destination must differ from its source")
        path.parent.mkdir(parents=True, exist_ok=True)
        descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8", newline="") as output:
                output.write(body)
                output.flush()
                os.fsync(output.fileno())
            os.chmod(temporary, 0o600)
            os.replace(temporary, path)
        except BaseException:
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass
            raise
    except OSError as error:
        fail(f"could not write candidate URL map {path}: {error}")


def scalar(value: str, label: str) -> str:
    value = value.strip()
    if not value or value[0] in "'\"" or " #" in value or "\t" in value:
        fail(f"{label} must be an unquoted scalar")
    return value


def sections(lines: list[str]) -> dict[str, tuple[int, int]]:
    starts: list[tuple[str, int]] = []
    seen: set[str] = set()
    for index, raw in enumerate(lines):
        line = raw.removesuffix("\n")
        match = TOP_LEVEL_KEY.fullmatch(line)
        if match is None:
            continue
        name = match.group(1)
        if name in seen:
            fail(f"URL map contains duplicate top-level key {name}")
        seen.add(name)
        starts.append((name, index))
    result: dict[str, tuple[int, int]] = {}
    for position, (name, start) in enumerate(starts):
        end = starts[position + 1][1] if position + 1 < len(starts) else len(lines)
        result[name] = (start, end)
    return result


def list_items(lines: list[str], section: str) -> list[list[str]]:
    ranges = sections(lines)
    if section not in ranges:
        return []
    start, end = ranges[section]
    header = lines[start].removesuffix("\n")
    match = TOP_LEVEL_KEY.fullmatch(header)
    if match is None or (match.group(2) or "").strip():
        fail(f"{section} must be a block sequence")
    result: list[list[str]] = []
    item_start: int | None = None
    for index in range(start + 1, end):
        line = lines[index].removesuffix("\n")
        if line.startswith("- "):
            if item_start is not None:
                result.append(lines[item_start:index])
            item_start = index
        elif item_start is None and line.strip():
            fail(f"{section} contains content outside a sequence item")
    if item_start is not None:
        result.append(lines[item_start:end])
    return result


def item_fields(item: list[str], label: str) -> dict[str, tuple[str, int]]:
    result: dict[str, tuple[str, int]] = {}
    for offset, raw in enumerate(item):
        line = raw.removesuffix("\n")
        match = FIRST_ITEM_FIELD.fullmatch(line) if offset == 0 else ITEM_FIELD.fullmatch(line)
        if match is None:
            continue
        name = match.group(1)
        if name in result:
            fail(f"{label} contains duplicate field {name}")
        result[name] = ((match.group(2) or "").strip(), offset)
    return result


def host_rule(item: list[str], index: int) -> tuple[list[str], str, set[str]]:
    label = f"hostRules[{index}]"
    fields = item_fields(item, label)
    if "hosts" not in fields or "pathMatcher" not in fields:
        fail(f"{label} must contain hosts and pathMatcher")
    if fields["hosts"][0]:
        fail(f"{label}.hosts must be a block sequence")
    host_offset = fields["hosts"][1]
    matcher_offset = fields["pathMatcher"][1]
    hosts: list[str] = []
    for offset, raw in enumerate(item):
        if offset in {host_offset, matcher_offset} or ITEM_FIELD.fullmatch(raw.removesuffix("\n")):
            continue
        match = HOST_VALUE.fullmatch(raw.removesuffix("\n"))
        if match is None:
            fail(f"{label} contains unsupported YAML")
        if not host_offset < offset < matcher_offset:
            fail(f"{label}.hosts contains an entry outside its hosts block")
        hosts.append(scalar(match.group(1), f"{label}.hosts"))
    if not hosts or len(hosts) != len(set(hosts)):
        fail(f"{label}.hosts must contain unique host names")
    matcher = scalar(fields["pathMatcher"][0], f"{label}.pathMatcher")
    return hosts, matcher, set(fields)


def path_matcher(item: list[str], index: int) -> tuple[str, str, set[str], int]:
    label = f"pathMatchers[{index}]"
    fields = item_fields(item, label)
    if "name" not in fields or "defaultService" not in fields:
        fail(f"{label} must contain name and defaultService")
    name = scalar(fields["name"][0], f"{label}.name")
    service = scalar(fields["defaultService"][0], f"{label}.defaultService")
    return name, service, set(fields), fields["defaultService"][1]


def parsed_routes(body: str) -> tuple[list[str], list[tuple[list[str], str, set[str]]], dict[str, tuple[str, set[str], list[str], int]]]:
    lines = body.splitlines(keepends=True)
    rules = [host_rule(item, index) for index, item in enumerate(list_items(lines, "hostRules"))]
    matchers: dict[str, tuple[str, set[str], list[str], int]] = {}
    for index, item in enumerate(list_items(lines, "pathMatchers")):
        name, service, fields, service_offset = path_matcher(item, index)
        if name in matchers:
            fail(f"URL map contains duplicate path matcher {name}")
        matchers[name] = (service, fields, item, service_offset)
    all_hosts: set[str] = set()
    for hosts, _, _ in rules:
        for host in hosts:
            if host in all_hosts:
                fail(f"URL map contains duplicate host rule for {host}")
            all_hosts.add(host)
    return lines, rules, matchers


def root_service(lines: list[str]) -> tuple[str, int]:
    ranges = sections(lines)
    if "defaultService" not in ranges:
        fail("URL map is missing defaultService")
    start, end = ranges["defaultService"]
    if end != start + 1:
        fail("top-level defaultService must be a scalar")
    match = TOP_LEVEL_KEY.fullmatch(lines[start].removesuffix("\n"))
    if match is None:
        fail("top-level defaultService is invalid")
    return scalar(match.group(2) or "", "defaultService"), start


def active_service(body: str, active_matcher: str) -> str:
    lines, _, matchers = parsed_routes(body)
    if active_matcher == ROOT_MATCHER:
        return root_service(lines)[0]
    if active_matcher not in matchers:
        fail(f"active path matcher {active_matcher} is missing")
    return matchers[active_matcher][0]


def validate_identifiers(active_matcher: str, canary_matcher: str, canary_host: str) -> None:
    if active_matcher != ROOT_MATCHER and NAME.fullmatch(active_matcher) is None:
        fail("active matcher name is invalid")
    if NAME.fullmatch(canary_matcher) is None or canary_matcher == active_matcher:
        fail("canary matcher name is invalid or collides with the active matcher")
    if HOST.fullmatch(canary_host) is None or ".." in canary_host:
        fail("canary host is invalid")


def canary_state(body: str, matcher_name: str, host: str, service_url: str) -> str:
    _, rules, matchers = parsed_routes(body)
    matching_hosts: list[tuple[list[str], str, set[str]]] = []
    matcher_uses = 0
    for hosts, matcher, fields in rules:
        if host in hosts:
            matching_hosts.append((hosts, matcher, fields))
        if matcher == matcher_name:
            matcher_uses += 1
    matcher = matchers.get(matcher_name)
    host_present = bool(matching_hosts)
    matcher_present = matcher is not None
    if not host_present and not matcher_present and matcher_uses == 0:
        return "absent"
    if not host_present or not matcher_present or matcher_uses != 1 or len(matching_hosts) != 1:
        fail("canary host rule and path matcher are only partially configured")
    hosts, host_matcher, host_fields = matching_hosts[0]
    matcher_service, matcher_fields, _, _ = matcher
    if hosts != [host] or host_matcher != matcher_name or host_fields != {"hosts", "pathMatcher"}:
        fail("canary host rule is not exclusively bound to the expected matcher")
    if matcher_service != service_url or matcher_fields != {"defaultService", "name"}:
        fail("canary path matcher does not exclusively reference the expected backend")
    return "ready"


def expected_reference_count(active_url: str, canary_url: str, target_url: str) -> int:
    return int(active_url == target_url) + int(canary_url == target_url)


def reference_count(body: str, target_url: str) -> int:
    count = 0
    for raw in body.splitlines():
        match = SCALAR_FIELD_VALUE.fullmatch(raw)
        if match is not None and match.group(1) == target_url:
            count += 1
    return count


def assert_state(
    body: str,
    active_matcher: str,
    expected_active_url: str,
    canary_matcher: str,
    canary_host: str,
    canary_url: str,
    forbidden_urls: list[str] | None = None,
) -> None:
    validate_identifiers(active_matcher, canary_matcher, canary_host)
    current = active_service(body, active_matcher)
    if current != expected_active_url:
        fail(f"active route points to {current}, expected {expected_active_url}")
    if canary_state(body, canary_matcher, canary_host, canary_url) != "ready":
        fail("canary route is missing")
    for url in {expected_active_url, canary_url}:
        expected = expected_reference_count(expected_active_url, canary_url, url)
        actual = reference_count(body, url)
        if actual != expected:
            fail(f"URL-map reference count for {url} is {actual}, expected {expected}")
    for url in forbidden_urls or []:
        if not url or url in {expected_active_url, canary_url}:
            fail("forbidden URL must be distinct from active and canary backends")
        if reference_count(body, url) != 0:
            fail(f"forbidden URL-map backend remains referenced: {url}")


def append_item(body: str, section: str, item: str) -> str:
    lines = body.splitlines(keepends=True)
    ranges = sections(lines)
    if section in ranges:
        _, end = ranges[section]
        lines.insert(end, item)
        return "".join(lines)
    if body and not body.endswith("\n"):
        body += "\n"
    return body + f"{section}:\n{item}"


def prepare_canary(body: str, active_matcher: str, expected_active_url: str, canary_matcher: str, canary_host: str, canary_url: str) -> str:
    validate_identifiers(active_matcher, canary_matcher, canary_host)
    current = active_service(body, active_matcher)
    if current != expected_active_url or expected_active_url == canary_url:
        fail("active route is not the distinct expected pre-cutover backend")
    state = canary_state(body, canary_matcher, canary_host, canary_url)
    if state == "absent":
        if reference_count(body, canary_url) != 0:
            fail("canary backend is already referenced outside the canary route")
        body = append_item(
            body,
            "hostRules",
            f"- hosts:\n  - {canary_host}\n  pathMatcher: {canary_matcher}\n",
        )
        body = append_item(
            body,
            "pathMatchers",
            f"- defaultService: {canary_url}\n  name: {canary_matcher}\n",
        )
    assert_state(body, active_matcher, expected_active_url, canary_matcher, canary_host, canary_url)
    return body


def rewrite_active(body: str, active_matcher: str, expected_current_url: str, new_url: str, canary_matcher: str, canary_host: str, canary_url: str) -> str:
    if expected_current_url == new_url:
        fail("active route source and destination must differ")
    assert_state(body, active_matcher, expected_current_url, canary_matcher, canary_host, canary_url)
    lines, _, matchers = parsed_routes(body)
    if active_matcher == ROOT_MATCHER:
        _, index = root_service(lines)
        lines[index] = f"defaultService: {new_url}\n"
    else:
        _, _, item, service_offset = matchers[active_matcher]
        old_line = item[service_offset]
        prefix = old_line.split("defaultService:", 1)[0]
        replacement = f"{prefix}defaultService: {new_url}\n"
        candidates = [
            index + service_offset
            for index in range(len(lines) - len(item) + 1)
            if lines[index : index + len(item)] == item
        ]
        if len(candidates) != 1:
            fail("could not locate active path matcher service line")
        lines[candidates[0]] = replacement
    rewritten = "".join(lines)
    assert_state(rewritten, active_matcher, new_url, canary_matcher, canary_host, canary_url)
    expected_old_count = int(expected_current_url == canary_url)
    if reference_count(rewritten, expected_current_url) != expected_old_count:
        fail("active route source backend remains referenced outside the canary route")
    return rewritten


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    active = commands.add_parser("active-backend")
    active.add_argument("source", type=Path)
    active.add_argument("active_matcher")
    for name in ("prepare-canary", "rewrite-active", "assert-state"):
        command = commands.add_parser(name)
        command.add_argument("source", type=Path)
        if name != "assert-state":
            command.add_argument("destination", type=Path)
        command.add_argument("active_matcher")
        command.add_argument("expected_active_url")
        if name == "rewrite-active":
            command.add_argument("new_url")
        command.add_argument("canary_matcher")
        command.add_argument("canary_host")
        command.add_argument("canary_url")
        if name == "assert-state":
            command.add_argument("--forbid-url", action="append", default=[])
    return parser


def main() -> None:
    args = build_parser().parse_args()
    body = read_map(args.source)
    if args.command == "active-backend":
        if args.active_matcher != ROOT_MATCHER and NAME.fullmatch(args.active_matcher) is None:
            fail("active matcher name is invalid")
        print(active_service(body, args.active_matcher))
    elif args.command == "prepare-canary":
        updated = prepare_canary(
            body,
            args.active_matcher,
            args.expected_active_url,
            args.canary_matcher,
            args.canary_host,
            args.canary_url,
        )
        write_map(args.destination, updated, args.source)
    elif args.command == "rewrite-active":
        updated = rewrite_active(
            body,
            args.active_matcher,
            args.expected_active_url,
            args.new_url,
            args.canary_matcher,
            args.canary_host,
            args.canary_url,
        )
        write_map(args.destination, updated, args.source)
    else:
        assert_state(
            body,
            args.active_matcher,
            args.expected_active_url,
            args.canary_matcher,
            args.canary_host,
            args.canary_url,
            args.forbid_url,
        )


if __name__ == "__main__":
    try:
        main()
    except RoutingError as error:
        print(f"url-map-routing: {error}", file=sys.stderr)
        raise SystemExit(1) from None
