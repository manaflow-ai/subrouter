#!/usr/bin/env python3
import importlib.util
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("check-runner-labels.py")
spec = importlib.util.spec_from_file_location("check_runner_labels", SCRIPT)
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)


class RunnerLabelTests(unittest.TestCase):
    def violations(self, workflow):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "workflow.yml"
            path.write_text(workflow, encoding="utf-8")
            return checker.violations([path])

    def test_new_hosted_labels_are_rejected(self):
        for label in ("windows-11-vs2026-arm", "xcode-27", "ubuntu-24.04-16core"):
            errors = self.violations(f"jobs:\n  build:\n    runs-on: {label}\n")
            self.assertEqual(len(errors), 1, (label, errors))

    def test_matrix_runner_values_are_checked(self):
        matrix_expression = "$" + "{{ matrix.name }}"
        errors = self.violations(
            "jobs:\n"
            "  build:\n"
            f"    runs-on: {matrix_expression}\n"
            "    strategy:\n"
            "      matrix:\n"
            "        name: [ubuntu-latest]\n"
        )
        self.assertEqual(len(errors), 1, errors)

    def test_safe_matrix_runner_passes(self):
        matrix_expression = "$" + "{{ matrix.name }}"
        errors = self.violations(
            "jobs:\n"
            "  build:\n"
            f"    runs-on: {matrix_expression}\n"
            "    strategy:\n"
            "      matrix:\n"
            "        name: [blacksmith-2vcpu-ubuntu-2404]\n"
        )
        self.assertEqual(errors, [])

    def test_only_manaflow_fork_branch_is_exempt(self):
        allowed = "$" + "{{ github.repository_owner != 'manaflow-ai' && 'ubuntu-latest' || 'blacksmith-2vcpu-ubuntu-2404' }}"
        rejected = "$" + "{{ github.repository_owner != 'other-owner' && 'ubuntu-latest' || 'blacksmith-2vcpu-ubuntu-2404' }}"
        self.assertEqual(self.violations(f"jobs:\n  build:\n    runs-on: {allowed}\n"), [])
        self.assertEqual(len(self.violations(f"jobs:\n  build:\n    runs-on: {rejected}\n")), 1)

    def test_exception_requires_a_reason(self):
        self.assertEqual(
            self.violations(
                "jobs:\n"
                "  build:\n"
                "    runs-on: ubuntu-latest # runner-policy-exception: trusted release\n"
            ),
            [],
        )
        self.assertEqual(
            len(self.violations("jobs:\n  build:\n    runs-on: ubuntu-latest # runner-policy-exception\n")),
            1,
        )


if __name__ == "__main__":
    unittest.main()
