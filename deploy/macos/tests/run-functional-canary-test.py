#!/usr/bin/env python3
from __future__ import annotations

import errno
import json
import hashlib
import importlib.util
import os
import select
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
RUNNER = ROOT / "deploy" / "macos" / "run-functional-canary.py"
SCHEMA = "subrouter.launchagent-functional-canary/v1"
LEG_SCHEMA = "subrouter.launchagent-functional-canary-leg/v1"
LEGS = (
    "peer-health-readiness",
    "authenticated-routed-codex",
    "sticky-reuse",
    "safe-failover-reuse",
    "authenticated-routed-claude",
    "existing-session-next-turn",
)
SOURCE_OID = "a" * 40
# Tests that snapshot the per-user lease directory the runner shares across
# all runs. Harnesses that run cases concurrently (cmd/subrouter's Go test)
# must run these alone, because any other runner adds lease files there.
SHARED_LEASE_DIRECTORY_TESTS = frozenset({
    "test_transaction_worker_path_and_hash_are_required_before_leases",
})
_active_runner: subprocess.Popen[str] | None = None


def _terminate_active_runner(_signum: int, _frame: object) -> None:
    if _active_runner is not None and _active_runner.poll() is None:
        try:
            os.killpg(_active_runner.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            _active_runner.wait(timeout=0.5)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(_active_runner.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            _active_runner.wait()
    os._exit(128 + signal.SIGTERM)


def process_exited(pid: int) -> bool:
    """True once pid is gone or is an unreaped zombie.

    A killed descendant in its own session is reparented to PID 1. Container
    inits that do not reap orphans leave it a zombie that still answers
    signal 0, so treat the Linux zombie state as exited. Without /proc (macOS)
    the signal probe alone decides.
    """
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return True
    try:
        stat = Path(f"/proc/{pid}/stat").read_bytes()
    except OSError:
        return False
    end = stat.rfind(b")")
    return end >= 0 and stat[end + 2:end + 3] == b"Z"


# A generous bound for waits whose expiry means the program under test is
# genuinely stuck, never merely slow on a loaded host. It is also the runner's
# own leg/total deadline in cases where those deadlines are not the property
# under test, so the runner cannot race the test's progress: MAX_TOTAL_TIMEOUT
# is 270s, above the Go wrapper's 240s per-case guard.
HANG_GUARD_SECONDS = 270


def wait_for_process_exit(pid: int, guard: float = HANG_GUARD_SECONDS) -> bool:
    """Block until pid exits (zombie or reaped); False only if the guard expires.

    The exit is observed as a kernel event rather than a sampling budget: a
    killed orphan is a zombie until launchd or a container init reaps it, and
    on a loaded host that can take arbitrarily long, so reaping is not waited for.
    """
    if hasattr(os, "pidfd_open"):
        try:
            descriptor = os.pidfd_open(pid)
        except ProcessLookupError:
            return True
        try:
            # A pidfd becomes readable once the process has exited, reaped or not.
            readable, _, _ = select.select([descriptor], [], [], guard)
            return bool(readable)
        finally:
            os.close(descriptor)
    if hasattr(select, "kqueue"):
        queue = select.kqueue()
        try:
            events = queue.control(
                [select.kevent(
                    pid,
                    select.KQ_FILTER_PROC,
                    select.KQ_EV_ADD | select.KQ_EV_ONESHOT,
                    select.KQ_NOTE_EXIT,
                )],
                1,
                guard,
            )
        finally:
            queue.close()
        for event in events:
            if event.flags & select.KQ_EV_ERROR:
                # Darwin cannot attach to a zombie or a reaped PID: both exited.
                if event.data == errno.ESRCH:
                    return True
                raise OSError(event.data, os.strerror(event.data))
            if event.fflags & select.KQ_NOTE_EXIT:
                return True
        return False
    deadline = time.monotonic() + guard
    while not process_exited(pid):
        if time.monotonic() >= deadline:
            return False
        time.sleep(0.05)
    return True


class FunctionalCanaryTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        self.config = self.root / "config.json"
        self.config.write_text("{}\n")
        self.config.chmod(0o600)
        self.success = self.root / "success.py"
        self.success.write_text(
            f"#!{sys.executable}\n"
            "import json, os\n"
            "print(json.dumps({'schema': '" + LEG_SCHEMA + "', "
            "'leg': os.environ['SUBROUTER_CANARY_LEG_NAME'], 'ok': True}))\n"
        )
        self.success.chmod(0o700)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def manifest(
        self,
        executable: Path | None = None,
        timeout: int = 10,
        total_timeout: int | None = None,
        manifest_name: str = "manifest.json",
        evidence_name: str = "evidence.json",
    ) -> Path:
        executable = executable or self.success
        digest = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
        body = {
            "schema": SCHEMA,
            "source_git_oid_unverified": SOURCE_OID,
            "candidate_worker": {"path": str(self.success), "sha256": digest(self.success)},
            "evidence_file": str(self.root / evidence_name),
            "total_timeout_seconds": total_timeout if total_timeout is not None else timeout * len(LEGS),
            "legs": [
                {
                    "name": name,
                    "executable": str(executable),
                    "executable_sha256": digest(executable),
                    "config_file": str(self.config),
                    "config_sha256": digest(self.config),
                    "timeout_seconds": timeout,
                }
                for name in LEGS
            ],
        }
        path = self.root / manifest_name
        path.write_text(json.dumps(body))
        path.chmod(0o600)
        return path

    def run_runner(
        self,
        manifest: Path,
        *arguments: str,
        environment: dict[str, str] | None = None,
        hang_guard: float = 60,
    ) -> subprocess.CompletedProcess[str]:
        global _active_runner
        runner_environment = dict(os.environ if environment is None else environment)
        runner_environment.setdefault(
            "SUBROUTER_CANARY_TRANSACTION_WORKER_PATH", str(self.success)
        )
        runner_environment.setdefault(
            "SUBROUTER_CANARY_TRANSACTION_WORKER_SHA256",
            hashlib.sha256(self.success.read_bytes()).hexdigest(),
        )
        process = subprocess.Popen(
            [sys.executable, str(RUNNER), "--manifest", str(manifest), *arguments],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            cwd=self.root,
            env=runner_environment,
            start_new_session=True,
        )
        _active_runner = process
        try:
            stdout, stderr = process.communicate(timeout=hang_guard)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            raise
        finally:
            _active_runner = None
        return subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr)

    def test_all_six_exact_legs_pass_and_write_bounded_evidence(self) -> None:
        manifest = self.manifest()
        validated = self.run_runner(manifest, "--validate-only")
        self.assertEqual(validated.returncode, 0, validated.stderr)
        completed = self.run_runner(manifest)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        evidence = json.loads((self.root / "evidence.json").read_text())
        self.assertEqual(evidence["status"], "passed")
        self.assertEqual(evidence["source_git_oid_unverified"], SOURCE_OID)
        self.assertEqual([leg["name"] for leg in evidence["legs"]], list(LEGS))
        self.assertNotIn(str(self.root), completed.stdout)

        repeated = self.run_runner(manifest)
        self.assertEqual(repeated.returncode, 0, repeated.stderr)

    def test_evidence_rejects_manifest_exact_path_and_existing_input_aliases(self) -> None:
        manifest = self.manifest()
        original = manifest.read_bytes()
        body = json.loads(original)
        body["evidence_file"] = str(manifest)
        manifest.write_text(json.dumps(body))
        exact = self.run_runner(manifest, "--validate-only")
        self.assertNotEqual(exact.returncode, 0)
        self.assertIn("aliases a canary input", exact.stderr)
        self.assertEqual(json.loads(manifest.read_text())["evidence_file"], str(manifest))

        helper = self.root / "helper.py"
        helper.write_bytes(self.success.read_bytes())
        helper.chmod(0o700)
        for name, executable, evidence in (
            ("candidate", None, self.success),
            ("helper", helper, helper),
            ("config", None, self.config),
        ):
            with self.subTest(name=f"exact-{name}"):
                exact_manifest = self.manifest(
                    executable, manifest_name=f"exact-{name}-manifest.json"
                )
                exact_body = json.loads(exact_manifest.read_text())
                exact_body["evidence_file"] = str(evidence)
                exact_manifest.write_text(json.dumps(exact_body))
                failed = self.run_runner(exact_manifest, "--validate-only")
                self.assertNotEqual(failed.returncode, 0)
                self.assertIn("aliases a canary input", failed.stderr)

        for name, source in (("manifest", manifest), ("config", self.config)):
            with self.subTest(name=name):
                fresh_manifest = self.manifest(manifest_name=f"{name}-manifest.json")
                alias = self.root / f"{name}-evidence.json"
                os.link(source if name == "config" else fresh_manifest, alias)
                fresh_body = json.loads(fresh_manifest.read_text())
                fresh_body["evidence_file"] = str(alias)
                if name == "manifest":
                    alias.unlink()
                    fresh_manifest.write_text(json.dumps(fresh_body))
                    os.link(fresh_manifest, alias)
                else:
                    fresh_manifest.write_text(json.dumps(fresh_body))
                failed = self.run_runner(fresh_manifest, "--validate-only")
                self.assertNotEqual(failed.returncode, 0)
                self.assertIn("aliases a canary input", failed.stderr)

    def test_evidence_never_overwrites_unrecognized_existing_file(self) -> None:
        manifest = self.manifest()
        evidence = self.root / "evidence.json"
        evidence.write_bytes(b"preserve-me\n")
        evidence.chmod(0o600)
        failed = self.run_runner(manifest)
        self.assertNotEqual(failed.returncode, 0)
        self.assertIn("recognized prior canary evidence", failed.stderr)
        self.assertEqual(evidence.read_bytes(), b"preserve-me\n")

    def test_manifest_digest_covers_the_exact_bytes_that_were_parsed(self) -> None:
        manifest = self.manifest()
        original = manifest.read_bytes()
        spec = importlib.util.spec_from_file_location("functional_canary_runner", RUNNER)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        parsed, _legs, digest = module._load_manifest(manifest)
        manifest.write_text("{}")
        self.assertEqual(parsed["schema"], SCHEMA)
        self.assertEqual(digest, hashlib.sha256(original).hexdigest())

    def test_stale_pid_start_identity_is_never_signaled(self) -> None:
        spec = importlib.util.spec_from_file_location("functional_canary_runner_identity", RUNNER)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])
        try:
            process_info = module._process_snapshot()[child.pid]
            identity = module._capture_identity(process_info)
            self.assertIsNotNone(identity)
            module._signal_matching_identities({child.pid: identity + " stale"}, signal.SIGTERM)
            time.sleep(0.05)
            self.assertIsNone(child.poll(), "stale PID identity was signaled")
            if sys.platform == "darwin":
                with mock.patch.object(
                    module.os,
                    "kill",
                    side_effect=AssertionError("numeric PID signaling is unsafe on Darwin"),
                ):
                    module._signal_matching_identities({child.pid: identity}, signal.SIGTERM)
            else:
                module._signal_matching_identities({child.pid: identity}, signal.SIGTERM)
            child.wait(timeout=2)
        finally:
            if child.poll() is None:
                child.kill()
                child.wait()

    def test_process_snapshot_never_attaches_new_identity_to_old_relation(self) -> None:
        spec = importlib.util.spec_from_file_location("functional_canary_runner_snapshot", RUNNER)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        pid = 4242
        candidate = subprocess.CompletedProcess([], 0, f"{pid}\n".encode(), b"")
        detailed = subprocess.CompletedProcess(
            [], 0, f"{pid} 1 {pid} Mon Jan 01 00:00:00 2024 command\n".encode(), b""
        )
        with (
            mock.patch.object(module.subprocess, "run", side_effect=[candidate, detailed]),
            mock.patch.object(module, "_process_start_identity", side_effect=["old", "replacement"]),
        ):
            self.assertNotIn(pid, module._process_snapshot())

    def test_leg_that_exits_before_identity_inspection_still_passes(self) -> None:
        spec = importlib.util.spec_from_file_location("functional_canary_runner_early_exit", RUNNER)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        real_identity = module._process_start_identity
        inspected: list[int] = []

        def identity_after_exit(pid: int, ps_started: str = "") -> str | None:
            # Hold the first inspection (the leg itself) until the leg is a
            # zombie, and then report it as Darwin's proc_pidinfo does.
            if inspected:
                return real_identity(pid, ps_started)
            inspected.append(pid)
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                state = subprocess.run(
                    ["/bin/ps", "-o", "state=", "-p", str(pid)],
                    stdout=subprocess.PIPE,
                    stderr=subprocess.DEVNULL,
                    text=True,
                ).stdout.strip()
                if state.startswith("Z"):
                    return None
                time.sleep(0.02)
            raise AssertionError("leg did not exit")

        leg = json.loads(self.manifest().read_text())["legs"][0]
        with mock.patch.object(module, "_process_start_identity", side_effect=identity_after_exit):
            result = module._run_leg(leg, "early-exit", time.monotonic() + 30)
        self.assertEqual(len(inspected), 1)
        self.assertEqual(result["name"], leg["name"])
        self.assertIs(result["ok"], True)

    def test_reaped_root_pid_reuse_cannot_seed_descendant_walk(self) -> None:
        spec = importlib.util.spec_from_file_location("functional_canary_runner_reaped", RUNNER)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        root_pid = 4242
        replacement = module._ProcessInfo(root_pid, 1, root_pid, "now", b"replacement", "new")
        unrelated_child = module._ProcessInfo(4243, root_pid, root_pid, "now", b"child", "child")
        module._tracked_child_identities[root_pid] = "old"
        with mock.patch.object(
            module,
            "_process_snapshot",
            return_value={root_pid: replacement, unrelated_child.pid: unrelated_child},
        ):
            module._refresh_tracked_descendants(root_pid)
        self.assertEqual(module._tracked_child_identities, {root_pid: "old"})

    def test_concurrent_runner_is_refused_without_mutating_winner_artifacts(self) -> None:
        gate = self.root / "winner.gate"
        log = self.root / "legs.log"
        self.config.write_text(json.dumps({"gate": str(gate), "log": str(log)}))
        blocking = self.root / "blocking.py"
        blocking.write_text(
            f"#!{sys.executable}\n"
            "import json, os, pathlib, time\n"
            "config=json.loads(pathlib.Path(os.environ['SUBROUTER_CANARY_LEG_CONFIG_FILE']).read_text())\n"
            "name=os.environ['SUBROUTER_CANARY_LEG_NAME']\n"
            "with open(config['log'],'a') as output: output.write(name+'\\n'); output.flush(); os.fsync(output.fileno())\n"
            "if name == 'peer-health-readiness':\n"
            "    while not pathlib.Path(config['gate']).exists(): time.sleep(0.02)\n"
            f"print(json.dumps({{'schema':'{LEG_SCHEMA}','leg':name,'ok':True}}))\n"
        )
        blocking.chmod(0o700)
        # Leg and total deadlines are not the property under test. The winner's
        # first leg stays blocked for as long as this test takes to start it and
        # run the contender, so its deadlines are hang guards that cannot fire
        # before the Go wrapper's own per-case guard.
        manifest = self.manifest(
            blocking,
            timeout=HANG_GUARD_SECONDS,
            total_timeout=HANG_GUARD_SECONDS,
            manifest_name="winner-manifest.json",
            evidence_name="winner-evidence.json",
        )
        contender_manifest = self.manifest(
            blocking,
            timeout=HANG_GUARD_SECONDS,
            total_timeout=HANG_GUARD_SECONDS,
            manifest_name="contender-manifest.json",
            evidence_name="contender-evidence.json",
        )
        first_tmp = self.root / "first-tmp"
        second_tmp = self.root / "second-tmp"
        first_tmp.mkdir(mode=0o700)
        second_tmp.mkdir(mode=0o700)
        binding = {
            "SUBROUTER_CANARY_TRANSACTION_WORKER_PATH": str(self.success),
            "SUBROUTER_CANARY_TRANSACTION_WORKER_SHA256": hashlib.sha256(
                self.success.read_bytes()
            ).hexdigest(),
        }
        owner_environment = dict(os.environ, TMPDIR=str(first_tmp), **binding)
        contender_environment = dict(os.environ, TMPDIR=str(second_tmp))
        owner = subprocess.Popen(
            [sys.executable, str(RUNNER), "--manifest", str(manifest)],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            cwd=self.root,
            env=owner_environment,
            start_new_session=True,
        )
        first_leg_entered = "peer-health-readiness\n"
        try:
            # Wait for the event itself: the winner's first leg has recorded
            # its entry (the runner published its evidence before starting
            # it), or the winner exited without getting there. Only a winner
            # that is genuinely stuck reaches the hang guard.
            deadline = time.monotonic() + HANG_GUARD_SECONDS
            evidence = self.root / "winner-evidence.json"
            while (
                owner.poll() is None
                and time.monotonic() < deadline
                and not (evidence.exists() and log.exists() and log.read_text() == first_leg_entered)
            ):
                time.sleep(0.02)
            if owner.poll() is not None:
                stdout, stderr = owner.communicate()
                self.fail(f"winner exited before entering its first leg: {stdout + stderr}")
            self.assertTrue(
                evidence.exists() and log.exists() and log.read_text() == first_leg_entered,
                "winner did not enter its first leg",
            )
            evidence_before = evidence.read_bytes()
            contender = self.run_runner(
                contender_manifest, environment=contender_environment, hang_guard=HANG_GUARD_SECONDS
            )
            self.assertNotEqual(contender.returncode, 0)
            self.assertIn("already running", contender.stderr)
            self.assertEqual(evidence.read_bytes(), evidence_before)
            self.assertFalse((self.root / "contender-evidence.json").exists())
            self.assertEqual(log.read_text().splitlines(), ["peer-health-readiness"])
            gate.write_text("continue\n")
            stdout, stderr = owner.communicate(timeout=HANG_GUARD_SECONDS)
            self.assertEqual(owner.returncode, 0, stdout + stderr)
            self.assertEqual(len(log.read_text().splitlines()), len(LEGS))
            resumed = self.run_runner(
                contender_manifest, environment=contender_environment, hang_guard=HANG_GUARD_SECONDS
            )
            self.assertEqual(resumed.returncode, 0, resumed.stderr)
            self.assertEqual(len(log.read_text().splitlines()), 2 * len(LEGS))
        finally:
            if owner.poll() is None:
                # The blocked leg runs in its own session, so killing the
                # winner's group alone would leak it polling for the gate
                # forever. Release the gate and let the runner's SIGTERM
                # handler terminate its leg before falling back to SIGKILL.
                gate.write_text("continue\n")
                try:
                    os.killpg(owner.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                try:
                    owner.communicate(timeout=HANG_GUARD_SECONDS)
                except subprocess.TimeoutExpired:
                    try:
                        os.killpg(owner.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    owner.communicate()

    def test_total_timeout_is_one_aggregate_deadline(self) -> None:
        log = self.root / "aggregate.log"
        self.config.write_text(json.dumps({"log": str(log)}))
        slow = self.root / "slow.py"
        slow.write_text(
            f"#!{sys.executable}\n"
            "import json, os, pathlib, time\n"
            "config=json.loads(pathlib.Path(os.environ['SUBROUTER_CANARY_LEG_CONFIG_FILE']).read_text())\n"
            "time.sleep(0.3)\n"
            "name=os.environ['SUBROUTER_CANARY_LEG_NAME']\n"
            "with open(config['log'],'a') as output: output.write(name+'\\n')\n"
            f"print(json.dumps({{'schema':'{LEG_SCHEMA}','leg':name,'ok':True}}))\n"
        )
        slow.chmod(0o700)
        completed = self.run_runner(self.manifest(slow, timeout=1, total_timeout=1))
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("total timeout exceeded", completed.stderr)
        evidence = json.loads((self.root / "evidence.json").read_text())
        self.assertEqual(evidence["status"], "failed")
        completed_legs = log.read_text().splitlines() if log.exists() else []
        self.assertLess(len(completed_legs), len(LEGS))

    def test_unknown_or_out_of_order_leg_fails_validation(self) -> None:
        manifest = self.manifest()
        body = json.loads(manifest.read_text())
        body["legs"][0]["name"] = "not-a-leg"
        manifest.write_text(json.dumps(body))
        failed = self.run_runner(manifest, "--validate-only")
        self.assertNotEqual(failed.returncode, 0)
        self.assertIn("missing, duplicated, or out of order", failed.stderr)

    def test_duplicate_manifest_field_fails_without_evidence_mutation(self) -> None:
        manifest = self.manifest()
        evidence = self.root / "evidence.json"
        evidence.write_bytes(b"preserve-me\n")
        evidence.chmod(0o600)
        raw = manifest.read_text().replace('"schema":', '"schema":"invalid-duplicate","schema":', 1)
        manifest.write_text(raw)
        failed = self.run_runner(manifest, "--validate-only")
        self.assertNotEqual(failed.returncode, 0)
        self.assertIn("duplicate field", failed.stderr)
        self.assertEqual(evidence.read_bytes(), b"preserve-me\n")

    def test_private_file_permissions_are_required(self) -> None:
        manifest = self.manifest()
        self.config.chmod(0o644)
        failed = self.run_runner(manifest, "--validate-only")
        self.assertNotEqual(failed.returncode, 0)
        self.assertIn("must not grant group or other permissions", failed.stderr)

    def test_normal_0755_executable_is_accepted_but_group_writable_is_not(self) -> None:
        self.success.chmod(0o755)
        accepted = self.run_runner(self.manifest(), "--validate-only")
        self.assertEqual(accepted.returncode, 0, accepted.stderr)
        self.success.chmod(0o775)
        rejected = self.run_runner(self.manifest(), "--validate-only")
        self.assertNotEqual(rejected.returncode, 0)
        self.assertIn("writable by group/other", rejected.stderr)

    def test_leg_executes_private_pinned_copies_of_executable_and_config(self) -> None:
        original_executable = self.root / "pin-check.py"
        original_executable.write_text(
            f"#!{sys.executable}\n"
            "import json, os, pathlib\n"
            f"original_executable = pathlib.Path({str(original_executable)!r})\n"
            f"original_config = pathlib.Path({str(self.config)!r})\n"
            "if pathlib.Path(__file__).resolve() == original_executable.resolve(): raise SystemExit(90)\n"
            "original_config.write_text('{\"changed\":true}\\n')\n"
            "observed = pathlib.Path(os.environ['SUBROUTER_CANARY_LEG_CONFIG_FILE']).read_text()\n"
            "original_config.write_text('{}\\n')\n"
            "if observed != '{}\\n': raise SystemExit(91)\n"
            "print(json.dumps({'schema': '" + LEG_SCHEMA + "', 'leg': os.environ['SUBROUTER_CANARY_LEG_NAME'], 'ok': True}))\n"
        )
        original_executable.chmod(0o755)
        completed = self.run_runner(self.manifest(original_executable))
        self.assertEqual(completed.returncode, 0, completed.stderr)

    def test_manifest_validation_matrix(self) -> None:
        cases = {
            "unknown field": lambda body: body.update({"unknown": True}),
            "boolean total": lambda body: body.update({"total_timeout_seconds": True}),
            "total above cap": lambda body: body.update({"total_timeout_seconds": 271}),
            "bad candidate hash": lambda body: body["candidate_worker"].update({"sha256": "f" * 64}),
            "bad executable hash": lambda body: body["legs"][0].update({"executable_sha256": "f" * 64}),
            "bad config hash": lambda body: body["legs"][0].update({"config_sha256": "f" * 64}),
        }
        for name, mutate in cases.items():
            with self.subTest(name=name):
                manifest = self.manifest()
                body = json.loads(manifest.read_text())
                mutate(body)
                manifest.write_text(json.dumps(body))
                failed = self.run_runner(manifest, "--validate-only")
                self.assertNotEqual(failed.returncode, 0)

        manifest = self.manifest()
        link = self.root / "manifest-link.json"
        link.symlink_to(manifest)
        self.assertNotEqual(self.run_runner(link, "--validate-only").returncode, 0)

        manifest = self.manifest()
        self.config.chmod(0o700)
        self.assertNotEqual(self.run_runner(manifest, "--validate-only").returncode, 0)

    def test_transaction_worker_path_and_hash_are_required_before_leases(self) -> None:
        manifest = self.manifest()
        evidence = self.root / "evidence.json"
        lease_directory = Path("/tmp") / f"subrouter-functional-canary-leases-{os.getuid()}"
        leases_before = set(lease_directory.glob("*.lock")) if lease_directory.exists() else set()
        base = dict(os.environ)
        cases = (
            (
                "missing",
                base,
                "migration transaction worker binding is required",
            ),
            (
                "path",
                dict(
                    base,
                    SUBROUTER_CANARY_TRANSACTION_WORKER_PATH=str(self.config),
                    SUBROUTER_CANARY_TRANSACTION_WORKER_SHA256=hashlib.sha256(
                        self.success.read_bytes()
                    ).hexdigest(),
                ),
                "candidate worker path does not match migration transaction",
            ),
            (
                "hash",
                dict(
                    base,
                    SUBROUTER_CANARY_TRANSACTION_WORKER_PATH=str(self.success),
                    SUBROUTER_CANARY_TRANSACTION_WORKER_SHA256="f" * 64,
                ),
                "candidate worker sha256 does not match migration transaction",
            ),
        )
        for name, environment, message in cases:
            with self.subTest(name=name):
                completed = subprocess.run(
                    [sys.executable, str(RUNNER), "--manifest", str(manifest)],
                    text=True,
                    capture_output=True,
                    cwd=self.root,
                    env=environment,
                    timeout=10,
                    check=False,
                )
                self.assertNotEqual(completed.returncode, 0)
                self.assertIn(message, completed.stderr)
                self.assertFalse(evidence.exists())
                leases_after = set(lease_directory.glob("*.lock")) if lease_directory.exists() else set()
                self.assertEqual(leases_after, leases_before)

    def test_transaction_worker_replacement_is_rejected_before_leases(self) -> None:
        manifest_path = self.manifest()
        original_hash = hashlib.sha256(self.success.read_bytes()).hexdigest()
        spec = importlib.util.spec_from_file_location("functional_canary_runner_binding", RUNNER)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        manifest, _legs, _digest = module._load_manifest(manifest_path)
        self.success.write_bytes(self.success.read_bytes() + b"# replacement\n")
        self.success.chmod(0o700)
        with mock.patch.dict(
            os.environ,
            {
                "SUBROUTER_CANARY_TRANSACTION_WORKER_PATH": str(self.success),
                "SUBROUTER_CANARY_TRANSACTION_WORKER_SHA256": original_hash,
            },
            clear=True,
        ):
            with self.assertRaisesRegex(
                module.CanaryError, "worker bytes do not match captured sha256"
            ):
                module._open_transaction_worker_binding(manifest)

    def test_transaction_worker_replacement_between_legs_is_rejected(self) -> None:
        self.config.write_text(json.dumps({"worker": str(self.success)}))
        replacing = self.root / "replace-worker.py"
        replacing.write_text(
            f"#!{sys.executable}\n"
            "import json, os, pathlib\n"
            "config=json.loads(pathlib.Path(os.environ['SUBROUTER_CANARY_LEG_CONFIG_FILE']).read_text())\n"
            "name=os.environ['SUBROUTER_CANARY_LEG_NAME']\n"
            "if name == 'peer-health-readiness':\n"
            "    worker=pathlib.Path(config['worker'])\n"
            "    replacement=worker.with_name(worker.name + '.replacement')\n"
            "    replacement.write_bytes(worker.read_bytes() + b'# replacement\\n')\n"
            "    replacement.chmod(0o700)\n"
            "    os.replace(replacement, worker)\n"
            f"print(json.dumps({{'schema':'{LEG_SCHEMA}','leg':name,'ok':True}}))\n"
        )
        replacing.chmod(0o700)
        completed = self.run_runner(self.manifest(replacing))
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("worker path identity changed", completed.stderr)
        evidence = json.loads((self.root / "evidence.json").read_text())
        self.assertEqual(evidence["status"], "failed")
        self.assertEqual(
            [leg["name"] for leg in evidence["legs"]],
            ["peer-health-readiness"],
        )

    def test_outer_bounded_callback_token_reaches_leg_environment(self) -> None:
        spec = importlib.util.spec_from_file_location("functional_canary_runner_token", RUNNER)
        self.assertIsNotNone(spec)
        self.assertIsNotNone(spec.loader)
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        token = "b" * 64
        with mock.patch.dict(
            os.environ, {"SUBROUTER_BOUNDED_CALLBACK_TOKEN": token}, clear=True
        ):
            environment = module._child_environment("leg", str(self.config), "run", "child")
        self.assertEqual(environment["SUBROUTER_BOUNDED_CALLBACK_TOKEN"], token)

    def test_all_legs_require_one_identical_reviewed_helper(self) -> None:
        other = self.root / "other.py"
        other.write_bytes(self.success.read_bytes() + b"\n")
        other.chmod(0o700)
        manifest = self.manifest()
        body = json.loads(manifest.read_text())
        body["legs"][2]["executable"] = str(other)
        body["legs"][2]["executable_sha256"] = hashlib.sha256(other.read_bytes()).hexdigest()
        manifest.write_text(json.dumps(body))
        failed = self.run_runner(manifest, "--validate-only")
        self.assertNotEqual(failed.returncode, 0)
        self.assertIn("one identical reviewed helper", failed.stderr)

    def test_missing_manifest_environment_fails_generically(self) -> None:
        completed = subprocess.run(
            [sys.executable, str(RUNNER)],
            text=True,
            capture_output=True,
            cwd=self.root,
            env={"HOME": str(self.root), "PATH": os.environ.get("PATH", "")},
            timeout=10,
            check=False,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertNotIn(str(self.root), completed.stderr)

    def test_executable_path_metacharacters_are_literal(self) -> None:
        literal = self.root / "probe;touch SHOULD_NOT_EXIST"
        literal.write_bytes(self.success.read_bytes())
        literal.chmod(0o700)
        completed = self.run_runner(self.manifest(literal))
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertFalse((self.root / "SHOULD_NOT_EXIST").exists())

    def test_child_secret_output_is_never_repeated(self) -> None:
        secret = "synthetic-secret-that-must-not-escape"
        failing = self.root / "failing.py"
        failing.write_text(
            f"#!{sys.executable}\n"
            f"print('{secret}')\n"
            f"raise SystemExit('{secret}')\n"
        )
        failing.chmod(0o700)
        completed = self.run_runner(self.manifest(failing))
        self.assertNotEqual(completed.returncode, 0)
        self.assertNotIn(secret, completed.stdout + completed.stderr)
        evidence = json.loads((self.root / "evidence.json").read_text())
        self.assertNotIn(secret, json.dumps(evidence))

    def test_timeout_kills_and_waits_for_descendant_process_group(self) -> None:
        pid_file = self.root / "descendant.pid"
        hanging = self.root / "hanging.py"
        hanging.write_text(
            f"#!{sys.executable}\n"
            "import signal, subprocess, sys, time\n"
            "signal.signal(signal.SIGTERM, signal.SIG_IGN)\n"
            "child_code = 'import signal,time; signal.signal(signal.SIGTERM, signal.SIG_IGN); time.sleep(30)'\n"
            "child = subprocess.Popen([sys.executable, '-c', child_code], start_new_session=True)\n"
            f"open({str(pid_file)!r}, 'w').write(str(child.pid))\n"
            "time.sleep(30)\n"
        )
        hanging.chmod(0o700)
        # The leg must time out after the fixture has started its detached
        # child. A short leg timeout keeps this fast; only when a heavily
        # loaded host could not even record the child in time does it retry
        # with enough startup budget. Either way the assertions below apply to
        # a run in which the child existed when the timeout fired.
        for attempt, timeout in enumerate((2, 15)):
            completed = self.run_runner(
                self.manifest(
                    hanging,
                    timeout=timeout,
                    manifest_name=f"manifest-{attempt}.json",
                    evidence_name=f"evidence-{attempt}.json",
                )
            )
            if pid_file.exists():
                break
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("timed out", completed.stderr)
        descendant = int(pid_file.read_text())
        for _ in range(30):
            if process_exited(descendant):
                break
            time.sleep(0.05)
        else:
            self.fail("timed-out descendant process remained alive")

    def test_timeout_does_not_kill_sibling_in_shared_runner_group(self) -> None:
        """A bounded callback may share its outer anchor group, not its leg groups."""
        hanging = self.root / "shared-group-hanging.py"
        hanging.write_text(
            f"#!{sys.executable}\n"
            "import time\n"
            "time.sleep(30)\n"
        )
        hanging.chmod(0o700)
        manifest = self.manifest(hanging, timeout=1, total_timeout=6)
        sibling_file = self.root / "sibling.pid"
        sibling_state = self.root / "sibling.state"
        runner_state = self.root / "runner.state"
        release = self.root / "release"
        wrapper = self.root / "shared-group-wrapper.py"
        wrapper.write_text(
            f"#!{sys.executable}\n"
            "import os, pathlib, signal, sys, time\n"
            "runner, manifest, sibling_file, sibling_state, runner_state, release = sys.argv[1:]\n"
            "anchor_pid = os.getpid()\n"
            "sibling_pid = os.fork()\n"
            "if sibling_pid == 0:\n"
            "    signal.signal(signal.SIGTERM, signal.SIG_IGN)\n"
            "    pathlib.Path(sibling_file).write_text(str(os.getpid()))\n"
            "    while True:\n"
            "        time.sleep(1)\n"
            "runner_pid = os.fork()\n"
            "if runner_pid == 0:\n"
            "    environment = dict(os.environ)\n"
            "    environment['SUBROUTER_BOUNDED_GROUP_ANCHOR_PID'] = str(anchor_pid)\n"
            "    os.execve(sys.executable, [sys.executable, runner, '--manifest', manifest], environment)\n"
            "_, runner_status = os.waitpid(runner_pid, 0)\n"
            "pathlib.Path(runner_state).write_text(str(runner_status))\n"
            "try:\n"
            "    waited, _ = os.waitpid(sibling_pid, os.WNOHANG)\n"
            "    pathlib.Path(sibling_state).write_text('alive' if waited == 0 else 'dead')\n"
            "except ChildProcessError:\n"
            "    pathlib.Path(sibling_state).write_text('dead')\n"
            "while not pathlib.Path(release).exists():\n"
            "    time.sleep(0.01)\n"
            "try:\n"
            "    os.kill(sibling_pid, signal.SIGTERM)\n"
            "except ProcessLookupError:\n"
            "    pass\n"
            "time.sleep(0.05)\n"
            "try:\n"
            "    os.kill(sibling_pid, signal.SIGKILL)\n"
            "except ProcessLookupError:\n"
            "    pass\n"
            "try:\n"
            "    os.waitpid(sibling_pid, 0)\n"
            "except ChildProcessError:\n"
            "    pass\n"
            "sys.exit(os.WEXITSTATUS(runner_status) if os.WIFEXITED(runner_status) else 125)\n"
        )
        wrapper.chmod(0o700)
        environment = dict(
            os.environ,
            SUBROUTER_CANARY_TRANSACTION_WORKER_PATH=str(self.success),
            SUBROUTER_CANARY_TRANSACTION_WORKER_SHA256=hashlib.sha256(
                self.success.read_bytes()
            ).hexdigest(),
        )
        process = subprocess.Popen(
            [sys.executable, str(wrapper), str(RUNNER), str(manifest), str(sibling_file),
             str(sibling_state), str(runner_state), str(release)],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            cwd=self.root,
            env=environment,
            start_new_session=True,
        )
        try:
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline and not sibling_file.exists():
                time.sleep(0.02)
            self.assertTrue(sibling_file.exists(), "shared-group sibling did not start")
            while time.monotonic() < deadline and not runner_state.exists():
                time.sleep(0.02)
            self.assertTrue(runner_state.exists(), "runner did not finish its timed-out leg")
            while time.monotonic() < deadline and not sibling_state.exists():
                time.sleep(0.02)
            self.assertEqual(sibling_state.read_text(), "alive")
            release.write_text("done\n")
            stdout, stderr = process.communicate(timeout=10)
            self.assertNotEqual(process.returncode, 0, stdout + stderr)
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()

    def test_exited_leg_leader_cannot_leave_descendant(self) -> None:
        pid_file = self.root / "detached.pid"
        leaking = self.root / "leaking.py"
        # The descendant never exits on its own, so once it is gone the runner
        # killed it: no sleep length has to outlast a slow runner, and waiting
        # for its exit event cannot mistake a natural exit for cleanup.
        leaking.write_text(
            f"#!{sys.executable}\n"
            "import json, os, subprocess, sys, time\n"
            "child=subprocess.Popen([sys.executable,'-c','import time\\nwhile True: time.sleep(3600)'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True); "
            f"open({str(pid_file)!r},'w').write(str(child.pid))\n"
            f"print(json.dumps({{'schema':'{LEG_SCHEMA}','leg':os.environ['SUBROUTER_CANARY_LEG_NAME'],'ok':True}}))\n"
        )
        leaking.chmod(0o700)
        try:
            # The leg exits at once; its deadlines are hang guards so a slow
            # host cannot turn the leaked-descendant failure into a timeout.
            completed = self.run_runner(
                self.manifest(leaking, timeout=HANG_GUARD_SECONDS, total_timeout=HANG_GUARD_SECONDS),
                hang_guard=HANG_GUARD_SECONDS,
            )
            self.assertNotEqual(completed.returncode, 0)
            self.assertIn("left descendant processes", completed.stderr)
            descendant = int(pid_file.read_text())
            # The runner signals the descendant before it exits; its death is
            # asynchronous, so wait for the kernel exit event, not a budget.
            if not wait_for_process_exit(descendant):
                self.fail("descendant from exited leader remained alive")
        finally:
            if pid_file.exists():
                leaked = int(pid_file.read_text())
                if not process_exited(leaked):
                    try:
                        os.kill(leaked, signal.SIGKILL)
                    except ProcessLookupError:
                        pass

    def test_malformed_oversized_and_wrong_leg_evidence_fail_closed(self) -> None:
        cases = {
            "malformed": "print('not json')\n",
            "wrong": "import json; print(json.dumps({'schema':'" + LEG_SCHEMA + "','leg':'wrong','ok':True}))\n",
            "oversized": "print('x' * 70000)\n",
            "duplicate": "print('{\"schema\":\"" + LEG_SCHEMA + "\",\"leg\":\"wrong\",\"leg\":\"peer-health-readiness\",\"ok\":true}')\n",
            "numeric-ok": "import json, os; print(json.dumps({'schema':'" + LEG_SCHEMA + "','leg':os.environ['SUBROUTER_CANARY_LEG_NAME'],'ok':1}))\n",
        }
        for name, body in cases.items():
            with self.subTest(name=name):
                executable = self.root / f"{name}.py"
                executable.write_text(f"#!{sys.executable}\n" + body)
                executable.chmod(0o700)
                completed = self.run_runner(self.manifest(executable))
                self.assertNotEqual(completed.returncode, 0)

    def test_post_validation_config_loss_is_redacted(self) -> None:
        secret = "synthetic-secret-config-name"
        config = self.root / secret
        config.write_text("{}\n")
        config.chmod(0o600)
        self.config = config
        deleting = self.root / "deleting.py"
        deleting.write_text(
            f"#!{sys.executable}\n"
            "import json, os\n"
            f"os.unlink({str(config)!r})\n"
            f"print(json.dumps({{'schema':'{LEG_SCHEMA}','leg':os.environ['SUBROUTER_CANARY_LEG_NAME'],'ok':True}}))\n"
        )
        deleting.chmod(0o700)
        manifest = self.manifest(deleting)
        completed = self.run_runner(manifest)
        self.assertNotEqual(completed.returncode, 0)
        combined = completed.stdout + completed.stderr + (self.root / "evidence.json").read_text()
        self.assertNotIn(secret, combined)
        self.assertNotIn("Traceback", combined)


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, _terminate_active_runner)
    fixture_pid_file = os.environ.get("SUBROUTER_CANARY_WRAPPER_TIMEOUT_PID_FILE")
    if fixture_pid_file:
        _active_runner = subprocess.Popen(
            [
                sys.executable,
                "-c",
                "import os,pathlib,signal,sys,time; "
                "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
                "pathlib.Path(sys.argv[1]).write_text(str(os.getpid())); time.sleep(60)",
                fixture_pid_file,
            ],
            start_new_session=True,
        )
        time.sleep(60)
    unittest.main()
