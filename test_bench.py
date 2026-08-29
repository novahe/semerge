import json
import os
import tempfile
import threading
import time
import unittest
from types import SimpleNamespace
from unittest import mock

import bench


class BenchConcurrencyTest(unittest.TestCase):
    def test_namespace_resolution_follows_temp_env_to_matching_conductor(self):
        with tempfile.TemporaryDirectory() as root:
            workdir = os.path.join(root, "nova-work")
            env_file = os.path.join(workdir, "smoke-tmp", "ws-bench-env.sh")
            conductor = os.path.join(
                workdir, "etc", "node-ctl-nova", "conductor.yaml")
            os.makedirs(os.path.dirname(env_file))
            os.makedirs(os.path.dirname(conductor))
            with open(conductor, "w", encoding="utf-8") as file:
                file.write("paths: { run_root: /nova/run, base_root: /nova/lib }\n")

            with mock.patch.dict(os.environ, {
                    "E2B_NAMESPACE": "",
                    "E2B_DOMAIN": "sandboxes.nova.local",
                    "E2B_API_URL": "https://api.sandboxes.nova.local:443",
            }, clear=False):
                namespace = bench._resolve_namespace(env_file)
                resolved_workdir = bench._resolve_workdir(env_file, namespace)
                resolved_conductor = bench._resolve_conductor_file(
                    env_file, namespace, resolved_workdir)

            self.assertEqual(namespace, "nova")
            self.assertEqual(resolved_workdir, workdir)
            self.assertEqual(resolved_conductor, conductor)

    def test_namespace_mismatch_fails_closed(self):
        with tempfile.TemporaryDirectory() as root:
            env_file = os.path.join(root, "nova-work", "tenant.env")
            with mock.patch.dict(os.environ, {
                    "E2B_NAMESPACE": "test",
                    "E2B_DOMAIN": "sandboxes.nova.local",
            }, clear=False):
                with self.assertRaisesRegex(RuntimeError, "namespace mismatch"):
                    bench._resolve_namespace(env_file)

    def test_cleanup_rejects_a_different_namespace(self):
        sid = "sid-other-ns"
        with tempfile.TemporaryDirectory() as root:
            paths = {
                "run": os.path.join(root, "run"),
                "base": os.path.join(root, "base"),
                "checkpoint": os.path.join(root, "checkpoint"),
            }
            for directory in paths.values():
                os.makedirs(os.path.join(directory, sid))

            with mock.patch.multiple(
                    bench,
                    RUNTIME_NAMESPACE="nova",
                    RUN_DIR=paths["run"],
                    BASE_DIR=paths["base"],
                    CHECKPOINT_DIR=paths["checkpoint"]):
                errors = bench.cleanup_sandbox_directories(sid, namespace="test")

            self.assertTrue(any("namespace mismatch" in error for error in errors))
            for directory in paths.values():
                self.assertTrue(os.path.isdir(os.path.join(directory, sid)))

    def test_cleanup_sandbox_directories_removes_only_sid_children(self):
        sid = "sid-cleanup"
        with tempfile.TemporaryDirectory() as root:
            paths = {
                "run": os.path.join(root, "run"),
                "base": os.path.join(root, "base"),
                "checkpoint": os.path.join(root, "checkpoint"),
            }
            for directory in paths.values():
                os.makedirs(os.path.join(directory, sid, "nested"))
                with open(os.path.join(directory, sid, "nested", "data"), "w") as file:
                    file.write("sample")
                os.makedirs(os.path.join(directory, "keep"))

            with mock.patch.multiple(
                    bench,
                    RUN_DIR=paths["run"],
                    BASE_DIR=paths["base"],
                    CHECKPOINT_DIR=paths["checkpoint"]):
                self.assertEqual(bench.cleanup_sandbox_directories(sid), [])

            for directory in paths.values():
                self.assertFalse(os.path.exists(os.path.join(directory, sid)))
                self.assertTrue(os.path.isdir(os.path.join(directory, "keep")))

    def test_run_round_cleans_directories_after_delete(self):
        sid = "sid-round-cleanup"
        response = SimpleNamespace(status_code=204)
        with tempfile.TemporaryDirectory() as root:
            paths = {
                "run": os.path.join(root, "run"),
                "base": os.path.join(root, "base"),
                "checkpoint": os.path.join(root, "checkpoint"),
            }
            for directory in paths.values():
                os.makedirs(os.path.join(directory, sid))

            with mock.patch.multiple(
                    bench,
                    RUN_DIR=paths["run"],
                    BASE_DIR=paths["base"],
                    CHECKPOINT_DIR=paths["checkpoint"]), \
                    mock.patch.object(bench, "create", return_value=(sid, "token")), \
                    mock.patch.object(bench, "probe", side_effect=[
                        (True, 1.0, "ok"), (True, 1.0, "ok")
                    ]), \
                    mock.patch.object(bench, "wait_launch_ms", return_value=(10.0, {})), \
                    mock.patch.object(bench.e2b_env, "api_request", return_value=response):
                result, failure = bench.run_round("template", "health")

            self.assertIsNone(failure)
            self.assertEqual(result["discarded"], 0)
            for directory in paths.values():
                self.assertFalse(os.path.exists(os.path.join(directory, sid)))

    def test_run_round_keeps_directories_when_delete_fails(self):
        sid = "sid-delete-failed"
        response = SimpleNamespace(status_code=500)
        with tempfile.TemporaryDirectory() as root:
            paths = {
                "run": os.path.join(root, "run"),
                "base": os.path.join(root, "base"),
                "checkpoint": os.path.join(root, "checkpoint"),
            }
            for directory in paths.values():
                os.makedirs(os.path.join(directory, sid))

            with mock.patch.multiple(
                    bench,
                    RUN_DIR=paths["run"],
                    BASE_DIR=paths["base"],
                    CHECKPOINT_DIR=paths["checkpoint"]), \
                    mock.patch.object(bench, "create", return_value=(sid, "token")), \
                    mock.patch.object(bench, "probe", side_effect=[
                        (True, 1.0, "ok"), (True, 1.0, "ok")
                    ]), \
                    mock.patch.object(bench, "wait_launch_ms", return_value=(10.0, {})), \
                    mock.patch.object(bench.e2b_env, "api_request", return_value=response):
                result, failure = bench.run_round("template", "health")

            self.assertEqual(result["discarded"], 1)
            self.assertEqual(result["samples"], [])
            self.assertIn("API delete HTTP 500", failure["error"])
            for directory in paths.values():
                self.assertTrue(os.path.isdir(os.path.join(directory, sid)))

    def test_create_injects_restore_prefetch_without_dropping_resource(self):
        response = SimpleNamespace(
            status_code=201,
            json=lambda: {
                "sandboxID": "sid-prefetch",
                "forwardAccessToken": "token-prefetch",
            },
        )
        with mock.patch.object(bench.e2b_env, "api_request", return_value=response) as request:
            sid, token = bench.create(
                "template",
                allocatable_memory="8GiB",
                restore_prefetch="memory",
            )

        self.assertEqual((sid, token), ("sid-prefetch", "token-prefetch"))
        body = request.call_args.kwargs["json"]
        self.assertEqual(
            json.loads(body["metadata"]["kuasar-sandbox.resource"]),
            {"allocatable": {"memory": "8GiB"}},
        )
        self.assertEqual(
            json.loads(body["metadata"]["kuasar-sandbox.restore"]),
            {"prefetch": "memory"},
        )

    def test_wait_launch_does_not_accept_launch_ms_while_starting(self):
        responses = [
            SimpleNamespace(status_code=200, json=lambda: {"state": "starting", "launchMs": 286.0}),
            SimpleNamespace(status_code=404, json=lambda: {}),
        ]
        with mock.patch.object(bench.e2b_env, "api_request", side_effect=responses):
            with self.assertRaises(bench.LaunchFailure) as raised:
                bench.wait_launch_ms("sid-failed", timeout=1)
        self.assertIn("HTTP 404", str(raised.exception))

    def test_wait_launch_accepts_only_running_launch(self):
        response = SimpleNamespace(
            status_code=200,
            json=lambda: {"state": "running", "launchMs": 123.5, "launchTiming": {"x": 1}},
        )
        with mock.patch.object(bench.e2b_env, "api_request", return_value=response):
            self.assertEqual(bench.wait_launch_ms("sid-ok", timeout=1), (123.5, {"x": 1}))

    def test_wait_launch_accepts_running_without_timing_patch(self):
        response = SimpleNamespace(
            status_code=200,
            json=lambda: {"state": "running"},
        )
        with mock.patch.object(bench.e2b_env, "api_request", return_value=response):
            self.assertEqual(bench.wait_launch_ms("sid-no-launch-ms"), (None, None))

    def test_run_round_accepts_running_without_launch_ms(self):
        response = SimpleNamespace(status_code=204)
        with mock.patch.object(bench, "create", return_value=("sid-no-patch", "token")), \
                mock.patch.object(bench, "probe", side_effect=[
                    (True, 12.0, "cold"), (True, 5.0, "warm")
                ]), \
                mock.patch.object(bench, "wait_launch_ms", return_value=(None, None)), \
                mock.patch.object(bench.e2b_env, "api_request", return_value=response):
            result, failure = bench.run_round("template", "health")

        self.assertIsNone(failure)
        self.assertEqual(result["discarded"], 0)
        self.assertEqual(len(result["e2e"]), 1)
        self.assertEqual(result["create"], [])
        self.assertEqual(result["cold"], [])
        self.assertEqual(result["warm"], [5.0])

    def test_failed_warm_probe_discards_entire_sample(self):
        timing = {"phasesMs": {"runner_wait": 8.0}, "points": {}}
        with mock.patch.object(bench, "create", return_value=("sid-failed", "token")), \
                mock.patch.object(bench, "probe", side_effect=[
                    (True, 12.0, ""), (False, 9.0, "warm failed")
                ]), \
                mock.patch.object(bench, "wait_launch_ms", return_value=(100.0, timing)), \
                mock.patch.object(bench.e2b_env, "api_request"):
            result, failure = bench.run_round("template", "health")

        self.assertEqual(result["discarded"], 1)
        self.assertEqual(result["samples"], [])
        for key in ("create", "create_api", "e2e", "cold", "warm"):
            self.assertEqual(result[key], [], key)
        self.assertEqual(result["phases"], {})
        self.assertEqual(failure["sid"], "sid-failed")

    def test_transaction_only_completes_on_first_health_without_warm_probe(self):
        response = SimpleNamespace(status_code=204)
        with mock.patch.object(bench, "create", return_value=("sid-qps", "token")), \
                mock.patch.object(bench, "probe", return_value=(True, 4.0, "ok")) as probe, \
                mock.patch.object(bench, "wait_launch_ms") as launch_timing, \
                mock.patch.object(bench.e2b_env, "api_request", return_value=response):
            result, failure = bench.run_round(
                "template", "health", transaction_only=True)

        self.assertIsNone(failure)
        self.assertEqual(result["discarded"], 0)
        self.assertEqual(len(result["qps_starts"]), 1)
        self.assertEqual(len(result["qps_completions"]), 1)
        self.assertEqual(len(result["samples"]), 1)
        self.assertEqual(result["warm"], [])
        probe.assert_called_once()
        launch_timing.assert_not_called()

    def test_throughput_metrics_use_completion_window(self):
        result = bench._new_results()
        result["qps_starts"] = [10.0, 11.0, 13.0]
        result["qps_completions"] = [12.0, 15.0]
        result["qps_started_at"] = 10.0
        result["qps_deadline"] = 20.0
        result["qps_finished_at"] = 20.0
        result["cleanup_failed"] = 1

        metrics = bench.throughput_metrics(result)

        self.assertEqual(metrics["attempted"], 3)
        self.assertEqual(metrics["accepted"], 2)
        self.assertEqual(metrics["discarded"], 1)
        self.assertEqual(metrics["measured_s"], 10.0)
        self.assertEqual(metrics["business_qps"], 0.2)
        self.assertAlmostEqual(metrics["success_rate"], 2 / 3)
        self.assertEqual(metrics["cleanup_failed"], 1)

    def test_run_throughput_aggregates_worker_results(self):
        def fake_worker(*_args):
            now = time.perf_counter()
            result = bench._new_results()
            result["qps_starts"].append(now)
            result["qps_completions"].append(now)
            result["qps_ends"].append(now)
            result["e2e"].append(10.0)
            result["samples"].append({
                "sid": "sid-throughput",
                "e2e": 10.0,
                "create_api": 1.0,
                "health": 2.0,
            })
            return result, []

        with mock.patch.object(bench, "_run_throughput_worker", side_effect=fake_worker):
            result = bench.run_throughput(
                "template", 0.01, "B", "health", concurrency=2)

        metrics = bench.throughput_metrics(result)
        self.assertEqual(metrics["attempted"], 2)
        self.assertEqual(metrics["accepted"], 2)
        self.assertEqual(metrics["discarded"], 0)

    def test_run_rounds_respects_worker_limit(self):
        lock = threading.Lock()
        active = 0
        peak = 0

        def fake_round(*_args):
            nonlocal active, peak
            with lock:
                active += 1
                peak = max(peak, active)
            time.sleep(0.02)
            with lock:
                active -= 1
            result = bench._new_results()
            result["samples"].append({"sid": "ok"})
            return result, None

        with mock.patch.object(bench, "run_round", side_effect=fake_round), \
                mock.patch.object(bench, "_show_progress"):
            result = bench.run_rounds("template", 7, "B", "health", concurrency=3)

        self.assertEqual(peak, 3)
        self.assertEqual(len(result["samples"]), 7)
        self.assertEqual(result["discarded"], 0)

    def test_round_mode_uses_fixed_size_waves(self):
        cases = ((1, 3), (2, 2), (4, 1))
        for concurrency, rounds in cases:
            with self.subTest(concurrency=concurrency, rounds=rounds):
                def fake_round(*_args):
                    result = bench._new_results()
                    result["samples"].append({"sid": "ok"})
                    return result, None

                with mock.patch.object(bench, "run_round", side_effect=fake_round), \
                        mock.patch.object(bench, "_show_progress"), \
                        mock.patch.object(bench, "_clear_progress"):
                    result = bench.run_rounds(
                        "template", None, "B", "health", concurrency=concurrency,
                        rounds=rounds)

                self.assertEqual(len(result["samples"]), concurrency * rounds)
                self.assertEqual(
                    [sample["round"] for sample in result["samples"]].count(1),
                    concurrency)
                self.assertEqual(
                    [sample["round"] for sample in result["samples"]].count(rounds),
                    concurrency)
                self.assertEqual(result["discarded"], 0)

    def test_round_mode_waits_for_a_complete_wave(self):
        concurrency = 3
        started = []
        lock = threading.Lock()
        first_wave_released = threading.Event()

        def fake_round(*_args):
            with lock:
                index = len(started)
                started.append(index)
            if index < concurrency:
                first_wave_released.wait(timeout=1)
            result = bench._new_results()
            result["samples"].append({"sid": str(index)})
            return result, None

        with mock.patch.object(bench, "run_round", side_effect=fake_round), \
                mock.patch.object(bench, "_show_progress"), \
                mock.patch.object(bench, "_clear_progress"):
            worker = threading.Thread(
                target=bench.run_rounds,
                args=("template", None, "B", "health"),
                kwargs={"concurrency": concurrency, "rounds": 2,
                        "round_interval": 0},
            )
            worker.start()
            deadline = time.time() + 1
            while time.time() < deadline:
                with lock:
                    if len(started) == concurrency:
                        break
                time.sleep(0.005)
            with lock:
                self.assertEqual(len(started), concurrency)
            first_wave_released.set()
            worker.join(timeout=1)

        self.assertFalse(worker.is_alive())
        self.assertEqual(len(started), concurrency * 2)

    def test_single_concurrency_does_not_print_self_comparison(self):
        result = bench._new_results()
        result["e2e"] = [100.0]
        result["samples"] = [{"sid": "sid-1"}]
        with mock.patch("builtins.print") as printed:
            bench.print_concurrency_trend("B", [(1, result)])
        printed.assert_not_called()

    def test_percentiles_are_interpolated_and_single_sample_is_exact(self):
        self.assertEqual(bench._p50([100.0]), 100.0)
        self.assertEqual(bench._p99([100.0]), 100.0)
        self.assertEqual(bench._p50([1.0, 3.0, 5.0, 7.0]), 4.0)
        self.assertAlmostEqual(bench._p99([1.0, 3.0, 5.0, 7.0]), 6.94)
        self.assertEqual(bench.stat_cells([]), ("-", "-", "-", "-", "-", "-"))
        self.assertEqual(len(bench.stat_cells([1.0, 2.0, 3.0])), 6)

    def test_multiple_concurrency_values_print_trend(self):
        first = bench._new_results()
        first["e2e"] = [100.0]
        first["samples"] = [{"sid": "sid-1"}]
        second = bench._new_results()
        second["e2e"] = [120.0]
        second["samples"] = [{"sid": "sid-2"}]
        with mock.patch("builtins.print") as printed:
            bench.print_concurrency_trend("B", [(1, first), (10, second)])
        self.assertTrue(any("B 并发趋势" in call.args[0] for call in printed.call_args_list))

    def test_success_details_are_verbose_only_but_counts_are_always_printed(self):
        result = bench._new_results()
        result["samples"] = [{
            "sid": "sid-1",
            "e2e": 100.0,
            "create_api": 10.0,
            "create": 70.0,
            "cold": 30.0,
            "warm": 5.0,
        }]
        result["discarded"] = 1

        cases = ((False, False), (True, True))
        for verbose, show_details in cases:
            with self.subTest(verbose=verbose):
                with mock.patch("builtins.print") as printed:
                    bench.print_sample_details(
                        "B", 50, result, verbose=verbose)
                output = "\n".join(
                    str(call.args[0]) for call in printed.call_args_list)
                self.assertEqual(
                    "=== B 每个成功沙箱" in output, show_details)
                self.assertIn("attempted=2", output)
                self.assertIn("accepted=1", output)
                self.assertIn("discarded=1", output)
                self.assertEqual("sid-1" in output, show_details)

    def test_failure_is_printed_without_verbose(self):
        failure = {
            "sid": "sid-failed",
            "error": "sid=sid-failed error=launch failed",
        }
        with mock.patch.object(
                bench, "run_round",
                return_value=(bench._new_results(), failure)), \
                mock.patch.object(bench, "_show_progress"), \
                mock.patch.object(bench, "_clear_progress"), \
                mock.patch("builtins.print") as printed:
            bench._run_sample_batch(
                "template", 1, "B", "health", bench.DEFAULT_RUN_COMMAND,
                "root", "/", 30, 1, None)

        output = "\n".join(
            str(call.args[0]) for call in printed.call_args_list)
        self.assertIn("sid-failed", output)
        self.assertIn("launch failed", output)

    def test_failure_log_is_jsonl_with_context(self):
        failure = {"sid": "sid-1", "error": "probe failed", "e2e_ms": 123.0}
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "failures.jsonl")
            bench.append_failure_log(path, "W", 30, "health", "template-1", failure)
            with open(path, encoding="utf-8") as log:
                record = json.loads(log.readline())

        self.assertEqual(record["variant"], "W")
        self.assertEqual(record["concurrency"], 30)
        self.assertEqual(record["sid"], "sid-1")
        self.assertEqual(record["template"], "template-1")

    def test_failure_log_can_record_round(self):
        failure = {"sid": "sid-2", "error": "launch failed"}
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "failures.jsonl")
            bench.append_failure_log(
                path, "B", 10, "health", "template-2", failure,
                round_number=3)
            with open(path, encoding="utf-8") as log:
                record = json.loads(log.readline())

        self.assertEqual(record["round"], 3)


if __name__ == "__main__":
    unittest.main()
