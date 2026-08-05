#!/usr/bin/env python3
"""Render and verify the Cloud Armor boundary for a public migration canary."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile
from typing import Any, NoReturn


ALLOW_PRIORITY = 900
DENY_PRIORITY = 1000
DEFAULT_PRIORITY = 2_147_483_647
TOKEN_HEADER = "x-subrouter-canary-token"
FORWARDED_HEADER = "X-Subrouter-Canary-Token"
REDACTED_VALUE = "canary-authorized"
POLICY_DESCRIPTION = "Subrouter front migration canary access boundary"
ALLOW_DESCRIPTION = "allow authenticated Subrouter front migration canary"
DENY_DESCRIPTION = "deny unauthenticated Subrouter front migration canary"
TOKEN = re.compile(r"[0-9a-f]{64}")
FINGERPRINT = re.compile(r"[A-Za-z0-9+/=_-]{8,256}")
NAME = re.compile(r"[a-z][a-z0-9-]{0,62}")
HOST = re.compile(r"[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?")
MAX_POLICY_BYTES = 2 * 1024 * 1024


class PolicyError(Exception):
    """The policy does not satisfy the migration access contract."""


def fail(message: str) -> NoReturn:
    raise PolicyError(message)


def allow_expression(host: str, token: str) -> str:
    return (
        f"{host_expression(host)} && "
        f"has(request.headers['{TOKEN_HEADER}']) && "
        f"request.headers['{TOKEN_HEADER}'] == '{token}'"
    )


def deny_expression(host: str) -> str:
    return host_expression(host)


def host_expression(host: str) -> str:
    return f"has(request.headers['host']) && request.headers['host'].lower() == '{host}'"


def default_rule() -> dict[str, Any]:
    return {
        "action": "allow",
        "description": "default rule",
        "match": {"config": {"srcIpRanges": ["*"]}, "versionedExpr": "SRC_IPS_V1"},
        "preview": False,
        "priority": DEFAULT_PRIORITY,
    }


def redaction_action() -> dict[str, Any]:
    return {
        "requestHeadersToAdds": [
            {"headerName": FORWARDED_HEADER, "headerValue": REDACTED_VALUE},
        ]
    }


def policy_document(name: str, host: str, token: str, fingerprint: str | None = None) -> dict[str, Any]:
    document = {
        "name": name,
        "description": POLICY_DESCRIPTION,
        "type": "CLOUD_ARMOR",
        "rules": [
            {
                "action": "allow",
                "description": ALLOW_DESCRIPTION,
                "headerAction": redaction_action(),
                "match": {"expr": {"expression": allow_expression(host, token)}},
                "preview": False,
                "priority": ALLOW_PRIORITY,
            },
            {
                "action": "deny(403)",
                "description": DENY_DESCRIPTION,
                "match": {"expr": {"expression": deny_expression(host)}},
                "preview": False,
                "priority": DENY_PRIORITY,
            },
            default_rule(),
        ],
    }
    if fingerprint is not None:
        document["fingerprint"] = fingerprint
    return document


def read_document(source: str) -> dict[str, Any]:
    try:
        if source == "-":
            body = sys.stdin.buffer.read(MAX_POLICY_BYTES + 1)
        else:
            path = Path(source)
            metadata = path.stat()
            if not stat.S_ISREG(metadata.st_mode) or metadata.st_size <= 0:
                fail("security policy input must be a non-empty regular file")
            if metadata.st_size > MAX_POLICY_BYTES:
                fail("security policy input exceeds the size limit")
            body = path.read_bytes()
        if not body or len(body) > MAX_POLICY_BYTES:
            fail("security policy input is empty or oversized")
        value = json.loads(body)
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        fail(f"could not read security policy: {error}")
    if isinstance(value, list):
        if len(value) != 1:
            fail("security policy describe output must contain exactly one policy")
        value = value[0]
    if not isinstance(value, dict):
        fail("security policy must be a JSON object")
    return value


def read_token(path: Path) -> str:
    try:
        metadata = path.stat()
        if path.is_symlink() or not stat.S_ISREG(metadata.st_mode) or stat.S_IMODE(metadata.st_mode) != 0o600:
            fail("canary token file must be a mode-0600 regular file, not a symlink")
        token = path.read_text(encoding="ascii").strip()
    except (OSError, UnicodeError) as error:
        fail(f"could not read canary token: {error}")
    if TOKEN.fullmatch(token) is None:
        fail("canary token must be 32 random bytes encoded as lowercase hex")
    return token


def write_document(destination: Path, value: dict[str, Any]) -> None:
    try:
        if destination.is_symlink():
            fail("security policy destination must not be a symlink")
        destination.parent.mkdir(parents=True, exist_ok=True)
        descriptor, temporary = tempfile.mkstemp(prefix=f".{destination.name}.", dir=destination.parent)
        try:
            os.fchmod(descriptor, 0o600)
            with os.fdopen(descriptor, "w", encoding="utf-8") as output:
                json.dump(value, output, separators=(",", ":"), sort_keys=True)
                output.write("\n")
                output.flush()
                os.fsync(output.fileno())
            os.replace(temporary, destination)
        except BaseException:
            try:
                os.unlink(temporary)
            except FileNotFoundError:
                pass
            raise
    except OSError as error:
        fail(f"could not write security policy: {error}")


def validate_identifiers(name: str, host: str, token: str | None = None) -> None:
    if NAME.fullmatch(name) is None:
        fail("security policy name is invalid")
    if HOST.fullmatch(host) is None or ".." in host:
        fail("canary host is invalid")
    if token is not None and TOKEN.fullmatch(token) is None:
        fail("canary token must be 32 random bytes encoded as lowercase hex")


def rule_expression(rule: dict[str, Any], label: str) -> str:
    match = rule.get("match")
    if not isinstance(match, dict) or set(match) != {"expr"}:
        fail(f"{label} must use one exact expression")
    expression = match.get("expr")
    if not isinstance(expression, dict) or not isinstance(expression.get("expression"), str):
        fail(f"{label} expression is invalid")
    return expression["expression"]


def validate_ready(document: dict[str, Any], name: str, host: str, expected_token: str | None) -> str:
    if document.get("name") != name or document.get("description") != POLICY_DESCRIPTION:
        fail("security policy identity does not match the managed canary policy")
    if document.get("type") != "CLOUD_ARMOR":
        fail("security policy must be a global Cloud Armor backend policy")
    rules = document.get("rules")
    if not isinstance(rules, list) or len(rules) != 3 or not all(isinstance(rule, dict) for rule in rules):
        fail("security policy must contain exactly allow, deny, and default rules")
    by_priority = {rule.get("priority"): rule for rule in rules}
    if set(by_priority) != {ALLOW_PRIORITY, DENY_PRIORITY, DEFAULT_PRIORITY}:
        fail("security policy priorities do not match the canary contract")
    allow = by_priority[ALLOW_PRIORITY]
    deny = by_priority[DENY_PRIORITY]
    default = by_priority[DEFAULT_PRIORITY]
    if (
        allow.get("action") != "allow"
        or allow.get("description") != ALLOW_DESCRIPTION
        or allow.get("headerAction") != redaction_action()
        or allow.get("preview", False) is not False
        or deny.get("action") != "deny(403)"
        or deny.get("description") != DENY_DESCRIPTION
        or deny.get("preview", False) is not False
        or rule_expression(deny, "deny rule") != deny_expression(host)
    ):
        fail("security policy canary allow or deny rule is invalid")
    expected_default = default_rule()
    for key, value in expected_default.items():
        if default.get(key) != value:
            fail("security policy default rule is invalid")
    expression = rule_expression(allow, "allow rule")
    if expected_token is not None:
        if expression != allow_expression(host, expected_token):
            fail("security policy allow rule does not use the expected canary token")
        token = expected_token
    else:
        template = re.escape(allow_expression(host, "{token}"))
        pattern = re.compile(rf"^{template.replace(re.escape('{token}'), '([0-9a-f]{64})')}$")
        match = pattern.fullmatch(expression)
        if match is None:
            fail("security policy allow rule is not bound to one strong canary token")
        token = match.group(1)
    return token


def main() -> None:
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    render = commands.add_parser("render")
    render.add_argument("destination", type=Path)
    render.add_argument("name")
    render.add_argument("host")
    render.add_argument("--token-file", type=Path, required=True)
    render.add_argument("--fingerprint")
    verify = commands.add_parser("assert-ready")
    verify.add_argument("source")
    verify.add_argument("name")
    verify.add_argument("host")
    verify.add_argument("--token-file", type=Path)
    args = parser.parse_args()
    token = read_token(args.token_file) if args.token_file is not None else None
    validate_identifiers(args.name, args.host, token)
    if args.command == "render":
        if token is None:
            fail("render requires a canary token")
        if args.fingerprint is not None and FINGERPRINT.fullmatch(args.fingerprint) is None:
            fail("security policy fingerprint is invalid")
        write_document(
            args.destination,
            policy_document(args.name, args.host, token, args.fingerprint),
        )
        return
    token = validate_ready(read_document(args.source), args.name, args.host, token)
    json.dump(
        {
            "name": args.name,
            "type": "CLOUD_ARMOR",
            "attached": True,
            "allow_priority": ALLOW_PRIORITY,
            "deny_priority": DENY_PRIORITY,
            "unauthorized_status": 403,
            "authorized_status": 400,
            "key_redacted_before_backend": True,
            "key_fingerprint_sha256": hashlib.sha256(token.encode()).hexdigest(),
        },
        sys.stdout,
        separators=(",", ":"),
        sort_keys=True,
    )
    sys.stdout.write("\n")


if __name__ == "__main__":
    try:
        main()
    except PolicyError as error:
        print(f"canary-security-policy: {error}", file=sys.stderr)
        raise SystemExit(1) from None
