"""Regression tests for the playground Helm deployment."""

from pathlib import Path
import subprocess
import unittest


CHART = Path(__file__).resolve().parents[1] / "helm/di-agent-system"


class PlaygroundDeploymentTests(unittest.TestCase):
    def test_dataset_controls_render_without_authentication_secret(self):
        rendered = subprocess.run(
            [
                "helm", "template", "test", str(CHART),
                "--show-only", "templates/playground.yaml",
                "--set", "playground.enabled=true",
                "--set", "playground.dataset.enabled=true",
            ],
            capture_output=True, text=True, check=True,
        ).stdout
        self.assertIn("location = /api/dataset/config", rendered)
        self.assertIn("limit_except PUT { deny all; }", rendered)
        self.assertIn("proxy_pass http://replay-streamer:8000/config", rendered)
        self.assertNotIn("auth_basic", rendered)
        self.assertNotIn("dataset-auth", rendered)


if __name__ == "__main__":
    unittest.main()
