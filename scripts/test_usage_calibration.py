import copy
import importlib.util
import json
import pathlib
import unittest

spec = importlib.util.spec_from_file_location(
    "inspector", pathlib.Path(__file__).with_name("inspect-usage-calibration.py"))
inspector = importlib.util.module_from_spec(spec)
spec.loader.exec_module(inspector)


def sample(sequence, account="private-account"):
    stamp = "2026-01-01T00:%02d:00Z" % sequence
    return {"schema_version": 1, "purpose": "quota_token_calibration",
            "worker_epoch": "private-worker", "sequence": sequence,
            "account_key": account, "account_generation": "generation-one",
            "provider": "claude", "plan_type": "max",
            "counters_sampled_at": stamp, "provider_observed_at": stamp,
            "dropped_observations": 0,
            "limitations": ["worker_local_completion_counters", "outside_server_usage_unverified"],
            "windows": [{"name": "5h", "used_percent": sequence,
                         "limit_window_seconds": 18000, "reset_at": "2026-01-01T05:00:00Z"}],
            "counters": [{"model": "example", "requests": sequence,
                          "requests_without_usage": 0, "input_tokens": sequence * 10,
                          "cached_input_tokens": sequence * 5,
                          "cache_write_input_tokens": 0, "output_tokens": sequence}]}


class InspectTests(unittest.TestCase):
    def test_descriptive_deltas_do_not_establish_quota_coverage(self):
        report = inspector.inspect([sample(2), sample(1)])
        interval = report["recent_intervals"][0]
        self.assertEqual(interval["counter_deltas"][0]["input_tokens"], 10)
        self.assertEqual(interval["window_deltas"][0]["used_percentage_point_delta"], 1)
        self.assertFalse(interval["eligible_for_quota_calibration"])
        self.assertIn("unsupported_demand_coverage", interval["exclusions"])
        self.assertNotIn("private-account", json.dumps(report))
        self.assertNotIn("private-worker", json.dumps(report))

    def test_sequence_gaps_are_global_to_worker_not_account(self):
        report = inspector.inspect([sample(1), sample(2, "other"), sample(3)])
        self.assertNotIn("sequence_gap", report["recent_intervals"][0]["exclusions"])
        report = inspector.inspect([sample(1), sample(3)])
        self.assertIn("sequence_gap", report["recent_intervals"][0]["exclusions"])

    def test_rejects_boundaries_missing_usage_and_drops(self):
        for field, value, reason in (
                ("worker_epoch", "other-worker", "worker_epoch_changed"),
                ("account_generation", "other-generation", "account_generation_changed"),
                ("dropped_observations", 1, "dropped_observations_changed")):
            second = sample(2)
            second[field] = value
            self.assertIn(reason, inspector.inspect([sample(1), second])["recent_intervals"][0]["exclusions"])
        second = sample(2)
        second["windows"][0]["reset_at"] = "2026-01-02T05:00:00Z"
        interval = inspector.inspect([sample(1), second])["recent_intervals"][0]
        self.assertIn("reset_identity_changed", interval["exclusions"])
        self.assertEqual(interval["window_deltas"], [])
        second = sample(2)
        second["counters"][0]["requests_without_usage"] = 1
        self.assertIn("missing_token_usage", inspector.inspect([sample(1), second])["recent_intervals"][0]["exclusions"])

    def test_rejects_duplicate_exports_and_invalid_counters(self):
        with self.assertRaises(ValueError):
            inspector.inspect([sample(1), copy.deepcopy(sample(1))])
        invalid = sample(1)
        invalid["counters"][0]["requests"] = -1
        with self.assertRaises(ValueError):
            inspector.inspect([invalid])


if __name__ == "__main__":
    unittest.main()
