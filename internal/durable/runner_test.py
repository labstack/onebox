import copy
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import runner as r


class Checkpoints(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.release = "20260906-120000-test"
        release_dir = self.root / "releases" / self.release
        release_dir.mkdir(parents=True)
        (release_dir / "compose.yaml").write_text("services: {}")
        (self.root / "current").symlink_to(release_dir)
        self.store = r.Store(self.root)
        self.config = {
            "application": "sample",
            "job": "refresh",
            "unit": "ob-sample-refresh",
            "container": "sample-refresh-1",
            "defaults": {"SOURCE": "catalog"},
            "env_files": [],
            "services": [],
            "retain_seconds": 3600,
            "timeout_seconds": 60,
            "steps": [
                self.step("sync", outputs=["RELEASE"]),
                self.step("index", inputs={"RELEASE_ID": "sync.RELEASE"}),
            ],
        }
        self.invocation = "a" * 32
        self.image = "sha256:" + "b" * 64
        self.docker = patch.object(r, "docker", return_value="")
        self.docker.start()
        self.addCleanup(self.docker.stop)
        self.running = patch.object(r, "container_running", return_value=False)
        self.running.start()
        self.addCleanup(self.running.stop)
        self.image_patch = patch.object(r, "image_identity", return_value=self.image)
        self.image_patch.start()
        self.addCleanup(self.image_patch.stop)

    def step(self, name, inputs=None, outputs=None):
        return {
            "name": name,
            "command": [name],
            "inputs": inputs or {},
            "outputs": outputs or [],
            "attempts": 1,
            "backoff": 1,
            "max_backoff": 1,
        }

    def prepare(self, resume="", invocation=None, overrides=None):
        return r.prepare(
            self.store,
            self.config,
            self.release,
            invocation or self.invocation,
            resume,
            "operation",
            overrides or {},
        )

    def test_prune_preserves_uncertain_running_execution(self):
        running = self.prepare()
        expired = self.prepare(invocation="c" * 32)
        for identity, state in [(running, "running"), (expired, "failed")]:
            value = self.store.read(identity)
            value["state"] = state
            value["expires_at"] = 1
            self.store.write(value)
        with self.store.locked():
            self.store.prune()
        self.assertTrue(self.store.record_path(running).exists())
        self.assertFalse(self.store.record_path(expired).exists())

    def test_run_status_counts_only_current_activation_attempts(self):
        identity = self.prepare()
        value = self.store.read(identity)
        path = self.root / "schedule" / "refresh.state"
        path.write_text("execution=" + identity + "\nattempt=1\n")
        value["steps"][0]["attempts"] = [{"invocation": "previous"}]
        value["steps"][1]["attempts"] = [
            {"invocation": self.invocation},
            {"invocation": self.invocation},
        ]
        r.update_run_status(self.store, value, self.invocation)
        self.assertEqual(path.read_text(), "execution=" + identity + "\nattempt=2\n")

    def fake_run(self, fail_index=False, invalid=False):
        def run(argv, **_kwargs):
            env = dict(
                argv[i + 1].split("=", 1) for i, arg in enumerate(argv) if arg == "-e"
            )
            step = argv[-1]
            self.calls.append((step, env))
            if step == "sync":
                output_dir = argv[argv.index("--volume") + 1].split(":")[0]
                Path(output_dir, "result.json").write_text(
                    json.dumps({"WRONG" if invalid else "RELEASE": "release-123"})
                )
            if step == "index":
                self.assertEqual(env["RELEASE_ID"], "release-123")
            return subprocess.CompletedProcess(
                argv, 1 if step == "index" and fail_index else 0
            )

        return run

    def test_resume_skips_committed_success_and_keeps_inputs_and_ids(self):
        identity = self.prepare(overrides={"SOURCE": "custom"})
        self.calls = []
        with patch.object(
            r.subprocess, "run", side_effect=self.fake_run(fail_index=True)
        ):
            self.assertEqual(r.execute(self.store, identity, self.invocation), 1)
        before = self.store.read(identity)
        self.assertEqual(before["state"], "failed")
        self.assertEqual(before["steps"][0]["outputs"], {"RELEASE": "release-123"})
        invocation = "c" * 32
        self.assertEqual(self.prepare(identity, invocation), identity)
        with patch.object(r.subprocess, "run", side_effect=self.fake_run()):
            self.assertEqual(r.execute(self.store, identity, invocation), 0)
        after = self.store.read(identity)
        self.assertEqual([name for name, _ in self.calls], ["sync", "index", "index"])
        self.assertEqual(after["state"], "succeeded")
        self.assertEqual(after["inputs"], {"SOURCE": "custom"})
        self.assertEqual(
            self.calls[1][1]["ONEBOX_STEP_ID"], self.calls[2][1]["ONEBOX_STEP_ID"]
        )
        self.assertNotEqual(
            self.calls[1][1]["ONEBOX_ATTEMPT_ID"], self.calls[2][1]["ONEBOX_ATTEMPT_ID"]
        )
        with self.assertRaisesRegex(ValueError, "terminal"):
            self.prepare(identity)

    def test_invalid_outputs_never_commit_success_or_run_next_step(self):
        identity = self.prepare()
        self.calls = []
        with patch.object(r.subprocess, "run", side_effect=self.fake_run(invalid=True)):
            self.assertEqual(r.execute(self.store, identity, self.invocation), 1)
        value = self.store.read(identity)
        self.assertEqual(value["steps"][0]["state"], "failed")
        self.assertEqual(value["steps"][0]["outputs"], {})
        self.assertEqual(value["steps"][1]["state"], "pending")

    def test_interrupted_attempt_and_activation_reconciled_on_resume(self):
        identity = self.prepare()
        value = self.store.read(identity)
        value["steps"][0].update(state="running", attempts=[{"state": "running"}])
        self.store.write(value)
        self.prepare(identity, "c" * 32)
        value = self.store.read(identity)
        self.assertEqual(value["steps"][0]["state"], "interrupted")
        self.assertEqual(value["activations"][0]["state"], "interrupted")

    def test_live_container_refuses_new_execution_and_resume(self):
        identity = self.prepare()
        with patch.object(r, "container_running", return_value=True):
            for resume in ["", identity]:
                with self.assertRaisesRegex(ValueError, "still running"):
                    self.prepare(resume)

    def test_resume_refuses_definition_image_generation_and_input_changes(self):
        identity = self.prepare()
        with self.assertRaisesRegex(ValueError, "override"):
            self.prepare(identity, overrides={"SOURCE": "changed"})
        changed = copy.deepcopy(self.config)
        self.config["steps"][0]["command"] = ["different"]
        with self.assertRaisesRegex(ValueError, "definition"):
            self.prepare(identity)
        self.config = changed
        with (
            patch.object(r, "image_identity", return_value="sha256:" + "d" * 64),
            self.assertRaisesRegex(ValueError, "image changed"),
        ):
            self.prepare(identity)
        r.main(["invalidate", str(self.root)])
        with self.assertRaisesRegex(ValueError, "changed"):
            self.prepare(identity)

    def test_expired_and_abandoned_records_cannot_resume(self):
        identity = self.prepare()
        value = self.store.read(identity)
        value["expires_at"] = 0
        self.store.write(value)
        with self.assertRaisesRegex(ValueError, "expired"):
            self.prepare(identity)
        r.abandon(self.store, identity)
        self.assertEqual(self.store.read(identity)["state"], "abandoned")

    def test_atomic_replace_failure_keeps_previous_checkpoint(self):
        identity = self.prepare()
        previous = self.store.read(identity)
        changed = copy.deepcopy(previous)
        changed["state"] = "failed"
        with (
            patch.object(r.os, "replace", side_effect=OSError("disk full")),
            self.assertRaises(OSError),
        ):
            self.store.write(changed)
        self.assertEqual(self.store.read(identity), previous)
        self.assertFalse(list(self.store.path.glob(".checkpoint-*")))

    def test_invalid_state_and_output_files_fail_closed(self):
        identity = self.prepare()
        path = self.store.record_path(identity)
        path.write_text("{broken")
        with self.assertRaises(ValueError):
            self.store.records()
        path.unlink()
        path.symlink_to(self.root / "current" / "compose.yaml")
        with self.assertRaises(OSError):
            self.store.records()
        path.unlink()
        path.write_text('{"key":"a","key":"b"}')
        with self.assertRaisesRegex(ValueError, "duplicate"):
            r.read_json(path)
        path.write_text("x" * 20)
        with self.assertRaisesRegex(ValueError, "limit"):
            r.read_json(path, 10)

    def test_interruption_never_starts_a_retry(self):
        self.config["steps"][0]["attempts"] = 3
        identity = self.prepare()
        with patch.object(
            r.subprocess, "run", side_effect=InterruptedError("SIGTERM")
        ) as run:
            self.assertEqual(r.execute(self.store, identity, self.invocation), 1)
        self.assertEqual(run.call_count, 1)
        value = self.store.read(identity)
        self.assertEqual(value["state"], "interrupted")
        self.assertEqual(len(value["steps"][0]["attempts"]), 1)

    def test_cleanup_refuses_another_invocations_container(self):
        row = {
            "Id": "container",
            "State": {"Running": True},
            "Config": {
                "Labels": {
                    "ob.execution.job": "refresh",
                    "ob.execution.invocation": "other",
                }
            },
        }
        with (
            patch.object(
                r, "docker", side_effect=["container", json.dumps([row])]
            ) as docker,
            self.assertRaisesRegex(ValueError, "another invocation"),
        ):
            r.cleanup_container(self.config, self.invocation)
        self.assertEqual(docker.call_count, 2)

    def test_nonfinite_retention_evidence_refused(self):
        identity = self.prepare()
        value = self.store.read(identity)
        value["expires_at"] = float("nan")
        self.store.record_path(identity).write_text(json.dumps(value))
        with self.assertRaisesRegex(ValueError, "non-finite"):
            r.main(["pins", str(self.root)])

    def test_service_restart_invalidates_resume(self):
        self.config["services"] = ["database"]
        row = {
            "Id": "same-container",
            "Image": "same-image",
            "State": {"Running": True, "StartedAt": "before"},
        }
        with patch.object(r, "docker", return_value=json.dumps([row])):
            identity = self.prepare()
        row["State"]["StartedAt"] = "after"
        with (
            patch.object(r, "docker", return_value=json.dumps([row])),
            self.assertRaisesRegex(ValueError, "changed"),
        ):
            self.prepare(identity)

    def test_output_limits_and_types(self):
        for value in [{"VALUE": 1}, {"VALUE": "a" * 4097}, {"VALUE": "\x00"}, {}]:
            with self.assertRaises(ValueError):
                r.validate_outputs(value, ["VALUE"])
        self.assertEqual(
            r.validate_outputs({"VALUE": 'quotes" and\nlines'}, ["VALUE"]),
            {"VALUE": 'quotes" and\nlines'},
        )


if __name__ == "__main__":
    unittest.main()
