"""Regression tests for worker deployment, peer registration and dataset selection."""

import json
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest
from unittest.mock import MagicMock, patch

ROOT = Path(__file__).resolve().parents[2]
PEERS = ROOT / "poc2/scripts/04-peers.sh"
ENTRYPOINT = ROOT / "semantic-map/go/entrypoint.sh"
CHART = ROOT / "poc2/helm/di-agent-system"

MOCK_KUBECTL = """#!/usr/bin/env python3
import hashlib
import json
import os
from pathlib import Path
import sys

root = Path(os.environ["MOCK_DIR"])
args = sys.argv[1:]
if "rollout" in args:
    sys.exit(0)
if "get" in args:
    print((root / "pods.json").read_text())
    sys.exit(0)
pod = args[args.index("exec") + 1]
url = next(a for a in args if a.startswith("http://"))
method = args[args.index("-X") + 1]
path = "/" + url.split("/", 3)[3]
state_file = root / "state.json"
state = json.loads(state_file.read_text()) if state_file.exists() else {}
peers = state.setdefault(pod, [])
with (root / "calls.jsonl").open("a") as stream:
    stream.write(json.dumps({"pod": pod, "method": method, "path": path, "url": url}) + "\\n")
if os.environ.get("FAIL_REQUEST"):
    sys.exit(22)
if path == "/healthz":
    print("{}")
    sys.exit(0)
if method == "GET":
    print(json.dumps(peers))
elif path == "/peers":
    peer_url = json.loads(args[args.index("-d") + 1])["url"]
    peer_id = hashlib.sha256(peer_url.encode()).hexdigest()[:16]
    if not any(p["url"] == peer_url for p in peers):
        peers.append({"id": peer_id, "url": peer_url})
    print(json.dumps({"id": peer_id}))
else:
    peer_id = path.split("/")[2]
    peer = next(p for p in peers if p["id"] == peer_id)
    peer["trust"] = json.loads(args[args.index("-d") + 1])["value"]
state_file.write_text(json.dumps(state))
"""


class AgentDeploymentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        kubectl = self.directory / "kubectl"
        kubectl.write_text(MOCK_KUBECTL)
        kubectl.chmod(0o755)
        self.env = {
            **os.environ,
            "PATH": str(self.directory) + os.pathsep + os.environ["PATH"],
            "MOCK_DIR": str(self.directory),
        }

    def pods(self, count=3, dataset_node="worker-1"):
        items = []
        for i in range(1, count + 1):
            items.append({
                "metadata": {"name": f"agent-{i}"},
                "spec": {
                    "nodeName": f"worker-{i}",
                    "containers": [{
                        "name": "di-agent",
                        "ports": [{"name": "http", "containerPort": 9191}],
                        "env": [{"name": "DATASET_NODE_NAME", "value": dataset_node}],
                    }],
                },
                "status": {
                    "hostIP": f"10.0.0.{i}",
                    "conditions": [{"type": "Ready", "status": "True"}],
                },
            })
        (self.directory / "pods.json").write_text(json.dumps({"items": items}))
        return items

    def run_peers(self, **env):
        return subprocess.run(
            ["bash", str(PEERS)], env={**self.env, **env},
            capture_output=True, text=True, timeout=15,
        )

    def test_three_agents_register_two_peers_each_and_rerun_is_safe(self):
        self.pods()
        for _ in range(2):
            result = self.run_peers()
            self.assertEqual(result.returncode, 0, result.stderr)
        state = json.loads((self.directory / "state.json").read_text())
        self.assertEqual(len(state), 3)
        for i in range(1, 4):
            peers = state[f"agent-{i}"]
            self.assertEqual(len(peers), 2)
            self.assertNotIn(f"http://10.0.0.{i}:9191", [p["url"] for p in peers])
            self.assertTrue(all(p["trust"] == 0.8 for p in peers))

    def test_missing_dataset_worker_fails_before_registration(self):
        self.pods(dataset_node="missing-worker")
        result = self.run_peers()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Dataset node", result.stderr)
        self.assertFalse((self.directory / "state.json").exists())

    def test_no_agents_is_an_error(self):
        self.pods(count=0)
        result = self.run_peers()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("No Helm-managed", result.stderr)

    def test_unready_agent_is_an_error(self):
        items = self.pods()
        items[0]["status"]["conditions"][0]["status"] = "False"
        (self.directory / "pods.json").write_text(json.dumps({"items": items}))
        result = self.run_peers()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Ready", result.stderr)

    def test_duplicate_node_identity_is_an_error(self):
        items = self.pods()
        items[1]["spec"]["nodeName"] = "worker-1"
        (self.directory / "pods.json").write_text(json.dumps({"items": items}))
        result = self.run_peers()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Multiple agents", result.stderr)

    def test_ipv6_peer_endpoint_is_bracketed(self):
        items = self.pods()
        items[0]["status"]["hostIP"] = "fd00::1"
        (self.directory / "pods.json").write_text(json.dumps({"items": items}))
        result = self.run_peers()
        self.assertEqual(result.returncode, 0, result.stderr)
        state = json.loads((self.directory / "state.json").read_text())
        self.assertIn("http://[fd00::1]:9191", [p["url"] for p in state["agent-2"]])

    def test_failed_http_request_is_not_reported_as_success(self):
        self.pods()
        result = self.run_peers(FAIL_REQUEST="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("verified", result.stdout)

    def test_stale_peer_is_reported(self):
        self.pods()
        (self.directory / "state.json").write_text(json.dumps({
            "agent-1": [{"id": "old", "url": "http://old-worker:9090"}],
        }))
        result = self.run_peers()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("expected", result.stderr)

    def test_only_designated_worker_generates_dataset_collector(self):
        # Stop before executing the installed binary; exercise the real config writer.
        script = ENTRYPOINT.read_text().replace(
            'exec /usr/local/bin/di-agent -config "${SPEC_FILE}" "$@"', "exit 0",
        )
        for node, designated, expected in [
            ("worker-1", "worker-1", True),
            ("worker-2", "worker-1", False),
            ("worker-3", "worker-1", False),
            ("worker-1", "", False),
        ]:
            with self.subTest(node=node, designated=designated):
                spec = self.directory / "agent.yaml"
                result = subprocess.run(
                    ["bash", "-c", script],
                    env={
                        **self.env, "NODE_ID": node, "DATASET_NODE_NAME": designated,
                        "DATASET_COLLECTOR_URL": "http://dataset-collector:8000/collect",
                        "NETDATA_URL": "", "SPEC_FILE": str(spec),
                    },
                    capture_output=True, text=True,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual("name: dataset" in spec.read_text(), expected)
                self.assertIn("name: cgroup", spec.read_text())

    def test_entrypoint_writes_rules_only_review(self):
        script = ENTRYPOINT.read_text().replace(
            'exec /usr/local/bin/di-agent -config "${SPEC_FILE}" "$@"', "exit 0",
        )
        for rules, expected in [
            ("composition,persistent-correlation", "      - composition\n      - persistent-correlation"),
            ("", "      []"),
        ]:
            with self.subTest(rules=rules):
                spec = self.directory / "rules.yaml"
                result = subprocess.run(
                    ["bash", "-c", script],
                    env={
                        **self.env, "SPEC_FILE": str(spec), "NETDATA_URL": "",
                        "PROPOSER": "true", "REVIEW_LEVEL": "rules", "REVIEW_RULES": rules,
                        "DECISION_BACKEND": "none", "REASONING_BACKEND": "none",
                    },
                    capture_output=True, text=True, timeout=15,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                config = spec.read_text()
                self.assertIn("enabled: true", config)
                self.assertIn('level: "rules"', config)
                self.assertIn(expected, config)
                self.assertIn('decision:\n      backend: "none"', config)
                self.assertIn('reasoning:\n      backend: "none"', config)

    def test_only_dataset_worker_waits_for_collector_readiness(self):
        rendered = subprocess.run(
            ["helm", "template", "test", str(CHART), "--show-only", "templates/agent.yaml",
             "--set", "agent.datasetNodeName=worker-1"],
            capture_output=True, text=True, check=True,
        ).stdout
        self.assertIn("name: wait-for-dataset-collector", rendered)
        script = textwrap.dedent(rendered.split("            - |\n", 1)[1].split("          env:", 1)[0])
        curl = self.directory / "curl"
        curl.write_text("""#!/usr/bin/env python3
import os
from pathlib import Path
import sys
path = Path(os.environ["MOCK_DIR"]) / "curl-count"
count = int(path.read_text()) + 1 if path.exists() else 1
path.write_text(str(count))
sys.exit(1 if count == 1 else 0)
""")
        curl.chmod(0o755)
        sleep = self.directory / "sleep"
        sleep.write_text("#!/bin/sh\nexit 0\n")
        sleep.chmod(0o755)
        for node, expected_calls in [("worker-2", 0), ("worker-1", 2)]:
            with self.subTest(node=node):
                result = subprocess.run(
                    ["/bin/sh", "-ec", script],
                    env={**self.env, "NODE_ID": node, "DATASET_NODE_NAME": "worker-1"},
                    capture_output=True, text=True, timeout=15,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                counter = self.directory / "curl-count"
                self.assertEqual(int(counter.read_text()) if counter.exists() else 0, expected_calls)
                if expected_calls:
                    self.assertIn("Waiting for dataset-collector readiness", result.stdout)
        for overrides in [[], ["--set", "datasetCollector.enabled=false", "--set", "agent.datasetNodeName=worker-1"]]:
            result = subprocess.run(
                ["helm", "template", "test", str(CHART), "--show-only", "templates/agent.yaml", *overrides],
                capture_output=True, text=True, check=True,
            )
            self.assertNotIn("initContainers:", result.stdout)

    def test_streamer_has_startup_grace_before_liveness(self):
        result = subprocess.run(
            ["helm", "template", "test", str(CHART), "--show-only", "templates/streamer.yaml"],
            capture_output=True, text=True, check=True,
        )
        startup = result.stdout.split("          startupProbe:\n", 1)[1].split("          readinessProbe:", 1)[0]
        self.assertIn("path: /status", startup)
        self.assertIn("failureThreshold: 60", startup)
        self.assertIn("periodSeconds: 5", startup)
        self.assertIn("timeoutSeconds: 3", startup)

    def test_agent_has_node_local_state_and_only_relevant_rollouts(self):
        def render(*overrides):
            result = subprocess.run(
                ["helm", "template", "test", str(CHART),
                 "--show-only", "templates/agent.yaml", *overrides],
                capture_output=True, text=True, check=True,
            )
            return result.stdout

        baseline = render()
        self.assertIn("mountPath: /var/lib/di-agent", baseline)
        self.assertIn('path: "/var/lib/di-agent-v6"', baseline)
        self.assertIn("type: DirectoryOrCreate", baseline)
        self.assertNotIn("diagent.io/restartedAt:", baseline)
        self.assertNotIn("diagent.io/config-checksum:", baseline)
        self.assertEqual(baseline, render())
        self.assertEqual(baseline, render("--set", "streamer.intervalMs=2000"))
        changed = render("--set", "agent.review.level=suggest")
        self.assertNotEqual(baseline, changed)
        self.assertIn('- name: REVIEW_LEVEL\n              value: "suggest"', changed)
        self.assertNotEqual(baseline, render("--set", "global.imageTag=test-build"))

    def test_entrypoint_rejects_malformed_rule_name(self):
        script = ENTRYPOINT.read_text().replace(
            'exec /usr/local/bin/di-agent -config "${SPEC_FILE}" "$@"', "exit 0",
        )
        result = subprocess.run(
            ["bash", "-c", script],
            env={
                **self.env, "SPEC_FILE": str(self.directory / "invalid.yaml"),
                "REVIEW_RULES": "composition,bad: rule",
            },
            capture_output=True, text=True, timeout=15,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Invalid REVIEW_RULES entry", result.stderr)

    def test_chart_renders_worker_daemonset_and_can_disable_agents(self):
        result = subprocess.run(
            ["helm", "template", "test", str(CHART), "--show-only", "templates/agent.yaml"],
            capture_output=True, text=True, check=True,
        )
        self.assertIn("kind: DaemonSet", result.stdout)
        for name, value in [
            ("PROPOSER", "true"),
            ("REVIEW_LEVEL", "rules"),
            ("REVIEW_RULES", "composition,persistent-correlation"),
            ("DECISION_BACKEND", "none"),
            ("REASONING_BACKEND", "none"),
        ]:
            self.assertIn(f'- name: {name}\n              value: "{value}"', result.stdout)
        self.assertNotIn("replicas:", result.stdout)
        self.assertIn("hostNetwork: true", result.stdout)
        self.assertIn("node-role.kubernetes.io/control-plane", result.stdout)
        self.assertIn("node-role.kubernetes.io/master", result.stdout)
        self.assertEqual(result.stdout.count("operator: DoesNotExist"), 2)
        self.assertIn("mountPath: /host/cgroup", result.stdout)
        self.assertIn("type: ClusterIP", result.stdout)
        disabled = subprocess.run(
            ["helm", "template", "test", str(CHART), "--set", "agent.enabled=false"],
            capture_output=True, text=True, check=True,
        )
        self.assertNotIn("kind: DaemonSet", disabled.stdout)

    def test_invalid_sharepoint_redirect_is_actionable_and_does_not_expose_link(self):
        for relative in ["poc2/data-ingest/ingest.py", "poc2/download_data.py"]:
            with self.subTest(module=relative):
                spec = importlib.util.spec_from_file_location("test_download", ROOT / relative)
                module = importlib.util.module_from_spec(spec)
                spec.loader.exec_module(module)
                opener = MagicMock()
                opener.open.return_value.geturl.return_value = "https://example.test/:f:/bad?e=sample"
                with (
                    patch.object(module.urllib.request, "build_opener", return_value=opener),
                    patch.dict(os.environ, {"DATA_URL": "https://example.test/:f:/bad"}),
                    self.assertRaises((ValueError, SystemExit)) as raised,
                ):
                    if hasattr(module, "download"):
                        module.download("https://example.test/:f:/bad")
                    else:
                        module.main()
                self.assertIn("did not resolve to a SharePoint folder", str(raised.exception))
                self.assertNotIn("example.test", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
