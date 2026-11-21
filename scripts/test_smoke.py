"""Exercise runner selection and failure reporting through its public CLI."""
from pathlib import Path
import subprocess
import sys
import unittest

SCRIPT = Path(__file__).with_name("smoke.py")


class SmokeRunnerTests(unittest.TestCase):
    def run_script(self, *args):
        return subprocess.run([sys.executable, str(SCRIPT), *args],
                              capture_output=True, text=True, timeout=15)

    def test_help_requires_no_go_or_binary(self):
        result = self.run_script("--help")
        self.assertEqual(result.returncode, 0)
        self.assertIn("--skip-race", result.stdout)
        self.assertIn("--cli-only", result.stdout)

    def test_all_skipped_is_not_success(self):
        result = self.run_script("--cli-only", "--skip-cli")
        self.assertEqual(result.returncode, 2)
        self.assertIn("all checks are skipped", result.stderr)

    def test_timeout_must_be_positive(self):
        result = self.run_script("--timeout", "0")
        self.assertEqual(result.returncode, 2)
        self.assertIn("must be positive", result.stderr)

    def test_failure_preserves_not_run_and_skip(self):
        result = self.run_script("--go", "/nonexistent/flintyard-go", "--skip-race")
        self.assertEqual(result.returncode, 1)
        self.assertIn("format: FAIL", result.stdout)
        self.assertIn("vet: NOT_RUN", result.stdout)
        self.assertIn("race: SKIP", result.stdout)
        self.assertIn("cli: NOT_RUN", result.stdout)

    def test_cli_only_does_not_require_go(self):
        result = self.run_script("/nonexistent/flintyard", "--cli-only",
                                 "--go", "/nonexistent/go")
        self.assertEqual(result.returncode, 1)
        self.assertIn("format: SKIP", result.stdout)
        self.assertIn("build: SKIP", result.stdout)
        self.assertIn("cli: FAIL", result.stdout)
        self.assertNotIn("START format", result.stdout)


if __name__ == "__main__":
    unittest.main()
