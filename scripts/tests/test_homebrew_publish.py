import base64
import importlib.util
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch


SCRIPTS = Path(__file__).resolve().parents[1] / "homebrew"
sys.path.insert(0, str(SCRIPTS))
spec = importlib.util.spec_from_file_location("homebrew_publish", SCRIPTS / "publish.py")
publisher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(publisher)
from generate import archive_names, read_checksums, render  # noqa: E402


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.manifest = self.root / "checksums.txt"
        self.manifest.write_text("".join(
            f"{index:064x}  {name}\n"
            for index, name in enumerate(archive_names("v0.7.0").values(), 1)
        ))
        self.checksums = read_checksums("v0.7.0", self.manifest.read_text())
        self.candidate = render("v0.7.0", self.checksums)
        self.formula = self.root / "gopls-mcp-proxy.rb"
        self.formula.write_text(self.candidate)

    def current(self, content):
        return {"type": "file", "encoding": "base64", "sha": "current-blob-sha",
                "content": base64.b64encode(content.encode()).decode()}

    def test_identical_retry_is_noop(self):
        with patch.object(publisher, "api", return_value=self.current(self.candidate)) as api:
            self.assertFalse(publisher.publish("v0.7.0", self.formula, self.manifest))
        self.assertEqual(api.call_count, 1)

    def test_upgrade_uses_current_sha_and_exact_tested_bytes(self):
        old = self.candidate.replace("0.7.0", "0.6.0")
        with patch.object(publisher, "api", side_effect=[self.current(old), {
            "commit": {"html_url": "https://github.com/fredgnr/homebrew-tap/commit/test"},
        }]) as api:
            self.assertTrue(publisher.publish("v0.7.0", self.formula, self.manifest))
        endpoint, payload = api.call_args.args
        self.assertEqual(endpoint, "repos/fredgnr/homebrew-tap/contents/Formula/gopls-mcp-proxy.rb")
        self.assertEqual(payload["branch"], "main")
        self.assertEqual(payload["sha"], "current-blob-sha")
        self.assertEqual(base64.b64decode(payload["content"]).decode(), self.candidate)

    def test_downgrade_never_writes(self):
        with patch.object(publisher, "api", return_value=self.current(
            self.candidate.replace("0.7.0", "0.10.0"),
        )) as api:
            with self.assertRaisesRegex(ValueError, "downgrade"):
                publisher.publish("v0.7.0", self.formula, self.manifest)
        self.assertEqual(api.call_count, 1)

    def test_changed_artifact_never_reads_or_writes_remote(self):
        self.formula.write_text(self.candidate + "# altered after testing\n")
        with patch.object(publisher, "api") as api:
            with self.assertRaisesRegex(ValueError, "Tested formula"):
                publisher.publish("v0.7.0", self.formula, self.manifest)
        api.assert_not_called()

    def test_concurrent_edit_error_is_not_retried_or_overwritten(self):
        old = self.candidate.replace("0.7.0", "0.6.0")
        failure = publisher.subprocess.CalledProcessError(1, ["gh", "api"])
        with patch.object(publisher, "api", side_effect=[self.current(old), failure]) as api:
            with self.assertRaises(publisher.subprocess.CalledProcessError):
                publisher.publish("v0.7.0", self.formula, self.manifest)
        self.assertEqual(api.call_count, 2)


if __name__ == "__main__":
    unittest.main()
