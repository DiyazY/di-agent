"""Regression tests for the playground Helm deployment."""

from pathlib import Path
import subprocess
import unittest


CHART = Path(__file__).resolve().parents[1] / "helm/di-agent-system"


class PlaygroundDeploymentTests(unittest.TestCase):
    def render(self, *overrides):
        return subprocess.run(
            [
                "helm", "template", "test", str(CHART),
                "--set", "playground.enabled=true",
                *overrides,
            ],
            capture_output=True, text=True,
        )

    def test_dataset_controls_use_cluster_ip_without_authentication_secret(self):
        result = self.render("--set", "playground.dataset.enabled=true")
        result.check_returncode()
        rendered = result.stdout
        playground_service = next(
            doc for doc in rendered.split("\n---\n")
            if "kind: Service\nmetadata:\n  name: playground\n" in doc
        )
        self.assertIn("type: ClusterIP", playground_service)
        self.assertNotIn("nodePort:", playground_service)
        self.assertIn("location = /api/dataset/config", rendered)
        self.assertIn("limit_except PUT { deny all; }", rendered)
        self.assertIn("proxy_pass http://replay-streamer:8000/config", rendered)
        self.assertNotIn("auth_basic", rendered)

    def test_collector_diagnostics_are_only_proxied_when_collector_is_enabled(self):
        for enabled in (True, False):
            with self.subTest(enabled=enabled):
                result = self.render(
                    "--set", "playground.dataset.enabled=true",
                    "--set", f"datasetCollector.enabled={str(enabled).lower()}",
                )
                result.check_returncode()
                rendered = result.stdout
                services = rendered.split("\n---\n")
                diagnostic_services = [
                    doc for doc in services
                    if "kind: Service\nmetadata:\n  name: dataset-collector-status\n" in doc
                ]
                self.assertEqual(len(diagnostic_services), int(enabled))
                self.assertEqual(
                    "proxy_pass http://dataset-collector-status:8000/ready" in rendered,
                    enabled,
                )
                if enabled:
                    self.assertIn("publishNotReadyAddresses: true", diagnostic_services[0])
                    service = next(
                        doc for doc in services
                        if "kind: Service\nmetadata:\n  name: dataset-collector\n" in doc
                    )
                    self.assertNotIn("publishNotReadyAddresses: true", service)

    def test_dataset_controls_require_streamer(self):
        result = self.render(
            "--set", "playground.dataset.enabled=true",
            "--set", "streamer.enabled=false",
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("playground.dataset.enabled requires streamer.enabled=true", result.stderr)

    def test_ship_only_playground_keeps_node_port(self):
        result = self.render("--set", "genset.enabled=true")
        result.check_returncode()
        playground_service = next(
            doc for doc in result.stdout.split("\n---\n")
            if "kind: Service\nmetadata:\n  name: playground\n" in doc
        )
        self.assertIn("type: NodePort", playground_service)
        self.assertNotIn("/api/dataset/config", result.stdout)


if __name__ == "__main__":
    unittest.main()
