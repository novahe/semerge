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
                kwargs={"concurrency": concurrency, "rounds": 2},
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
