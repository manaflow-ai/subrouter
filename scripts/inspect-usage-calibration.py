#!/usr/bin/env python3
"""Inspect private calibration intervals without estimating quota weights."""

import argparse
import datetime as dt
import json
import math
import sys
from collections import Counter

COUNTERS = ("requests", "requests_without_usage", "input_tokens",
            "cached_input_tokens", "cache_write_input_tokens", "output_tokens")


def timestamp(value):
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamp has no timezone")
    return parsed


def inspect(samples, limit=20):
    # Ordering is based on when local counters were copied, not a provider's
    # potentially cached observation time or interleaved append order.
    samples = sorted(samples, key=lambda s: timestamp(s["counters_sampled_at"]))
    previous = {}
    worker_sequences = {}
    worker_gaps = Counter()
    exclusions = Counter()
    intervals = []
    seen_sequences = set()
    for sample in samples:
        if sample["schema_version"] != 1 or sample["purpose"] != "quota_token_calibration":
            raise ValueError("unsupported calibration schema")
        worker, sequence = sample["worker_epoch"], sample["sequence"]
        account = sample["account_key"]
        if not worker or not account or not sample["account_generation"]:
            raise ValueError("missing generation or account key")
        if not isinstance(sequence, int) or isinstance(sequence, bool) or sequence <= 0:
            raise ValueError("invalid sequence")
        if (worker, sequence) in seen_sequences:
            raise ValueError("duplicate worker sequence; overlapping exports are unsupported")
        seen_sequences.add((worker, sequence))
        for counter in sample["counters"]:
            for field in COUNTERS:
                value = counter[field]
                if not isinstance(value, int) or isinstance(value, bool) or value < 0:
                    raise ValueError("invalid cumulative counter")
            if counter["requests_without_usage"] > counter["requests"]:
                raise ValueError("invalid missing usage count")
        if len({c["model"] for c in sample["counters"]}) != len(sample["counters"]):
            raise ValueError("duplicate model counters")
        if len({w["name"] for w in sample["windows"]}) != len(sample["windows"]):
            raise ValueError("duplicate quota windows")
        if worker in worker_sequences and sequence != worker_sequences[worker] + 1:
            worker_gaps[worker] += 1
        worker_sequences[worker] = sequence
        before = previous.get(account)
        previous[account] = (sample, worker_gaps[worker])
        if before is None:
            continue
        prior, prior_gaps = before
        reasons = set()
        if worker != prior["worker_epoch"]:
            reasons.add("worker_epoch_changed")
        elif worker_gaps[worker] != prior_gaps:
            reasons.add("sequence_gap")
        for field, reason in (("account_generation", "account_generation_changed"),
                              ("provider", "provider_changed"), ("plan_type", "plan_changed")):
            if sample[field] != prior[field]:
                reasons.add(reason)
        if sample["dropped_observations"] != prior["dropped_observations"]:
            reasons.add("dropped_observations_changed")
        if timestamp(sample["provider_observed_at"]) <= timestamp(prior["provider_observed_at"]):
            reasons.add("provider_observation_not_newer")
        if timestamp(sample["counters_sampled_at"]) <= timestamp(prior["counters_sampled_at"]):
            reasons.add("counter_time_not_newer")
        limitations = sorted(set(sample["limitations"] + prior["limitations"]))
        # Schema 1 supplies worker-local counters and explicitly unverified
        # outside/error demand. It has no evidence of complete quota coverage.
        reasons.add("unsupported_demand_coverage")
        if "counter_capacity_or_overflow" in limitations:
            reasons.add("incomplete_counters")
        old_counters = {c["model"]: c for c in prior["counters"]}
        new_counters = {c["model"]: c for c in sample["counters"]}
        deltas = []
        for model in sorted(old_counters.keys() | new_counters.keys()):
            delta = {"model": model}
            for field in COUNTERS:
                old = old_counters.get(model, {}).get(field, 0)
                new = new_counters.get(model, {}).get(field, 0)
                if any(not isinstance(v, int) or isinstance(v, bool) or v < 0 for v in (old, new)):
                    raise ValueError("invalid cumulative counter")
                delta[field] = new - old
                if new < old:
                    reasons.add("counter_reset_or_missing_model")
            if delta["requests_without_usage"]:
                reasons.add("missing_token_usage")
            deltas.append(delta)
        if not deltas or sum(d["requests"] for d in deltas) <= 0:
            reasons.add("no_completed_requests")
        old_windows = {w["name"]: w for w in prior["windows"]}
        new_windows = {w["name"]: w for w in sample["windows"]}
        if old_windows.keys() != new_windows.keys():
            reasons.add("window_set_changed")
        windows = []
        for name in sorted(old_windows.keys() & new_windows.keys()):
            old, new = old_windows[name], new_windows[name]
            reset = new["reset_at"]
            if not reset or reset.startswith("0001-"):
                reasons.add("unknown_reset_identity")
            elif timestamp(reset) != timestamp(old["reset_at"]):
                reasons.add("reset_identity_changed")
            if new["limit_window_seconds"] <= 0 or new["limit_window_seconds"] != old["limit_window_seconds"]:
                reasons.add("window_duration_unknown_or_changed")
            delta = new["used_percent"] - old["used_percent"]
            if not math.isfinite(delta):
                raise ValueError("invalid window percentage")
            if delta < 0:
                reasons.add("window_replenished_or_reset")
            windows.append({"name": name, "used_percentage_point_delta": delta,
                            "reset_at": reset})
        if not windows:
            reasons.add("no_comparable_windows")
        if reasons & {"worker_epoch_changed", "account_generation_changed", "provider_changed",
                      "plan_changed", "counter_reset_or_missing_model"}:
            deltas = []  # These counters do not have a comparable baseline.
        if reasons & {"reset_identity_changed", "unknown_reset_identity", "window_set_changed",
                      "window_duration_unknown_or_changed", "window_replenished_or_reset"}:
            windows = []
        exclusions.update(reasons)
        intervals.append({"provider": sample["provider"],
                          "from": prior["counters_sampled_at"],
                          "to": sample["counters_sampled_at"],
                          "provider_observed_from": prior["provider_observed_at"],
                          "provider_observed_to": sample["provider_observed_at"],
                          "eligible_for_quota_calibration": False,
                          "exclusions": sorted(reasons), "limitations": limitations,
                          "window_deltas": windows, "counter_deltas": deltas})
    return {"samples": len(samples), "intervals": len(intervals),
            "eligible_intervals": 0, "exclusion_counts": dict(sorted(exclusions.items())),
            "recent_intervals": intervals[-limit:] if limit else []}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("files", nargs="*", help="JSONL files; stdin if omitted")
    parser.add_argument("--limit", type=int, default=20, help="recent intervals to display")
    args = parser.parse_args()
    if args.limit < 0:
        parser.error("limit must be nonnegative")
    try:
        samples = []
        if args.files:
            for path in args.files:
                with open(path) as source:
                    samples.extend(json.loads(line) for line in source if line.strip())
        else:
            samples = [json.loads(line) for line in sys.stdin if line.strip()]
        json.dump(inspect(samples, args.limit), sys.stdout, indent=2, allow_nan=False)
        print()
    except (ValueError, KeyError, TypeError, OSError):
        # Malformed private input can contain credentials; do not echo values.
        parser.exit(1, "Invalid or unreadable calibration input; no report generated.\n")


if __name__ == "__main__":
    main()
