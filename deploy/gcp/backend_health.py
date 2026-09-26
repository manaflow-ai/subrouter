"""Shared validation for Google Cloud backend health responses."""

from __future__ import annotations

import json
from typing import Any


def healthy_backend_membership(value: Any) -> tuple[str, ...] | None:
    if not isinstance(value, list) or not value:
        return None
    identities: list[str] = []
    for backend in value:
        if not isinstance(backend, dict):
            return None
        backend_id = backend.get("backend")
        if not isinstance(backend_id, str) or not backend_id:
            return None
        status = backend.get("status")
        if not isinstance(status, dict):
            return None
        health_statuses = status.get("healthStatus")
        if not isinstance(health_statuses, list) or not health_statuses:
            return None
        for item in health_statuses:
            if not isinstance(item, dict) or item.get("healthState") != "HEALTHY":
                return None
            instance = item.get("instance")
            ip_address = item.get("ipAddress")
            port = item.get("port")
            if (
                not isinstance(instance, str)
                or not instance
                or not isinstance(ip_address, str)
                or not ip_address
                or isinstance(port, bool)
                or not isinstance(port, int)
                or port <= 0
                or port > 65535
            ):
                return None
            identities.append(
                json.dumps(
                    {
                        "backend": backend_id,
                        "instance": instance,
                        "ip_address": ip_address,
                        "port": port,
                    },
                    separators=(",", ":"),
                    sort_keys=True,
                )
            )
    membership = tuple(sorted(identities))
    if not membership or len(set(membership)) != len(membership):
        return None
    return membership
