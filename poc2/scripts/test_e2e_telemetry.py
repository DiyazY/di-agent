import json
import subprocess
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import e2e_telemetry


class TelemetryCheckTest(unittest.TestCase):
    def run_check(self, *, late_other=False):
        clock = [0.0]
        attempts = [0]
        other_checks = [0]
        subject = "engine:ci-test"
        sample = {
            "id": f"ci_speed@{subject}", "subject": subject, "value": 42,
            "unit": "rpm", "range": [0, 100], "range_declared": True,
            "labels": {"origin": "real", "dataset_source": "short"},
        }

        def kubectl(*args, input_text=None):
            if args[:2] == ("get", "pods"):
                return json.dumps({"items": [
                    {"spec": {"nodeName": node}, "metadata": {"name": node}}
                    for node in ("di-agent-smoke-worker", "other1", "other2")
                ]})
            if input_text:
                attempts[0] += 1
                if attempts[0] == 1:
                    raise subprocess.CalledProcessError(1, args, stderr="broker not ready")
                if attempts[0] == 2:
                    raise subprocess.TimeoutExpired(args, 30)
                return ""
            return "result,table,t_source,value\n,0,1,42\n"

        def agent_request(pod, path):
            if pod == "di-agent-smoke-worker":
                return {"properties": [sample]}
            other_checks[0] += 1
            return {"properties": [sample] if late_other and other_checks[0] > 2 else []}

        def sleep(seconds):
            clock[0] += seconds

        with patch.object(e2e_telemetry, "kubectl", side_effect=kubectl), \
             patch.object(e2e_telemetry, "agent_request", side_effect=agent_request), \
             patch.object(e2e_telemetry.uuid, "uuid4", return_value=SimpleNamespace(hex="test")), \
             patch.object(e2e_telemetry.time, "monotonic", side_effect=lambda: clock[0]), \
             patch.object(e2e_telemetry.time, "sleep", side_effect=sleep):
            e2e_telemetry.main()
        return attempts[0], other_checks[0]

    def test_retries_producer_failures_and_checks_other_agents_twice(self):
        attempts, other_checks = self.run_check()
        self.assertGreaterEqual(attempts, 4)
        self.assertEqual(other_checks, 4)

    def test_rejects_late_ingestion_on_another_agent(self):
        with self.assertRaisesRegex(AssertionError, "incorrectly collected"):
            self.run_check(late_other=True)


if __name__ == "__main__":
    unittest.main()
