"""Release authenticity, stable-version ordering, and immutable formula safeguards."""

import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]


def load(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / "scripts/homebrew" / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


GENERATE = load("generate")
with patch.dict(sys.modules, {"generate": GENERATE}):
    PREPARE = load("prepare")


def template():
    lines = ['class GoplsMcpProxy < Formula', '  version "@VERSION@"']
    for target in GENERATE.TARGETS:
        lines.extend([
            f'  url "https://github.com/{GENERATE.REPOSITORY}/releases/download/'
            f'@TAG@/gopls-mcp-proxy_@TAG@_{target}.tar.gz"',
            f'  sha256 "@SHA256_{target.upper()}@"',
        ])
    return "\n".join([*lines, "end", ""])


def checksums(tag="v0.7.0"):
    return {target: hashlib.sha256(target.encode()).hexdigest() for target in GENERATE.TARGETS}


def manifest(tag="v0.7.0", values=None):
    values = values or checksums(tag)
    return "".join(f"{values[target]}  {name}\n"
                   for target, name in GENERATE.archive_names(tag).items())


class FormulaTests(unittest.TestCase):
    def test_stable_versions_are_strict_and_numerically_ordered(self):
        self.assertGreater(GENERATE.stable_version("v0.10.0"), GENERATE.stable_version("v0.9.9"))
        for value in ("v01.2.3", "v1.02.3", "v1.2.03", "1.2.3", "v1.2", "v1.2.3-rc.1",
                      "v1.2.3+build.1", "dev-123456789abc", "v1.2.3\n", "../v1.2.3"):
            with self.subTest(tag=value), self.assertRaises(ValueError):
                GENERATE.stable_version(value)

    def test_complete_manifest_accepts_sha256sum_text_and_binary_modes(self):
        expected = checksums()
        self.assertEqual(GENERATE.read_checksums("v0.7.0", manifest()), expected)
        binary = manifest().replace("  gopls", " *gopls")
        self.assertEqual(GENERATE.read_checksums("v0.7.0", binary), expected)
        reversed_lines = "\n".join(reversed(manifest().splitlines()))
        self.assertEqual(GENERATE.read_checksums("v0.7.0", reversed_lines), expected)

    def test_manifest_rejects_missing_duplicate_extra_malformed_and_traversal_entries(self):
        source = manifest()
        lines = source.splitlines()
        invalid = ["", "\n".join(lines[:-1]), source + lines[0], source + "\n",
                   source.replace("gopls-mcp-proxy_", "../gopls-mcp-proxy_", 1),
                   source.replace("v0.7.0", "v0.6.0", 1), source.replace("  ", " ", 1),
                   source.replace(lines[0][:64], "z" * 64, 1)]
        for value in invalid:
            with self.subTest(manifest=value), self.assertRaises(ValueError):
                GENERATE.read_checksums("v0.7.0", value)

    def test_render_resolves_exact_version_platform_and_checksum(self):
        result = GENERATE.render("v0.7.0", checksums(), template())
        self.assertIn('version "0.7.0"', result)
        self.assertNotIn("@", result)
        for target, name in GENERATE.archive_names("v0.7.0").items():
            self.assertIn(name, result)
            self.assertIn(checksums()[target], result)

    def test_repository_template_round_trips_through_immutable_guard(self):
        result = GENERATE.render("v0.7.0", checksums())
        GENERATE.guard_current("v0.7.0", checksums(), result)
        self.assertNotIn("@SHA256_", result)
        self.assertNotIn("@TAG@", result)

    def test_template_or_hash_corruption_fails_closed(self):
        for source in (template() + "@UNKNOWN@", template().replace("@VERSION@", "0.7.0")):
            with self.assertRaises(ValueError):
                GENERATE.render("v0.7.0", checksums(), source)
        for values in ({}, {**checksums(), "linux_arm64": "not-a-checksum"}):
            with self.assertRaises(ValueError):
                GENERATE.render("v0.7.0", values, template())

    def test_same_version_allows_formula_packaging_changes_but_never_asset_changes(self):
        current = GENERATE.render("v0.7.0", checksums(), template())
        GENERATE.guard_current("v0.7.0", checksums(), current + "# packaging fix\n")
        modified = {**checksums(), "darwin_arm64": "0" * 64}
        with self.assertRaisesRegex(ValueError, "changed release checksum"):
            GENERATE.guard_current("v0.7.0", modified, current)

    def test_rollback_is_rejected_and_upgrade_uses_numeric_order(self):
        current = GENERATE.render("v0.9.0", checksums(), template())
        with self.assertRaisesRegex(ValueError, "downgrade"):
            GENERATE.guard_current("v0.7.0", checksums(), current)
        GENERATE.guard_current("v0.10.0", checksums(), current)

    def test_unknown_existing_formula_fails_closed(self):
        current = GENERATE.render("v0.7.0", checksums(), template())
        for invalid in ("", current.replace('version "0.7.0"', ''),
                        current + 'version "0.7.0"\n',
                        current.replace("github.com/fredgnr", "example.com/fredgnr"),
                        current.replace("darwin_arm64.tar.gz", "darwin_amd64.tar.gz")):
            with self.subTest(formula=invalid), self.assertRaises(ValueError):
                GENERATE.guard_current("v0.7.0", checksums(), invalid)

    def test_rejected_generation_preserves_existing_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            checksum_file = root / "checksums.txt"
            checksum_file.write_text(manifest(), encoding="utf-8")
            output = root / "formula.rb"
            current = GENERATE.render("v0.8.0", checksums(), template())
            output.write_text(current, encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "downgrade"):
                GENERATE.generate("v0.7.0", checksum_file, output, output)
            self.assertEqual(output.read_text(encoding="utf-8"), current)


class PrepareTests(unittest.TestCase):
    def setUp(self):
        self.tag = "v0.7.0"
        self.payloads = {
            name: f"archive bytes for {target}".encode()
            for target, name in GENERATE.archive_names(self.tag).items()
        }
        values = {target: hashlib.sha256(self.payloads[name]).hexdigest()
                  for target, name in GENERATE.archive_names(self.tag).items()}
        self.payloads["checksums.txt"] = manifest(self.tag, values).encode()
        self.release = {
            "tag_name": self.tag, "draft": False, "prerelease": False,
            "assets": [{
                "name": name, "state": "uploaded", "size": len(payload),
                "digest": f"sha256:{hashlib.sha256(payload).hexdigest()}",
                "browser_download_url": f"https://github.com/{GENERATE.REPOSITORY}/releases/download/{self.tag}/{name}",
            } for name, payload in self.payloads.items()],
        }

    def mock_gh(self, *args, **kwargs):
        if args[0] == "api":
            self.assertEqual(args, ("api", f"repos/{GENERATE.REPOSITORY}/releases/tags/{self.tag}"))
            return json.dumps(self.release)
        self.assertEqual(args[:3], ("release", "download", self.tag))
        self.assertEqual(args[args.index("--repo") + 1], GENERATE.REPOSITORY)
        requested = [args[i + 1] for i, item in enumerate(args) if item == "--pattern"]
        self.assertEqual(set(requested), set(self.payloads))
        output = Path(args[args.index("--dir") + 1])
        for name, payload in self.payloads.items():
            (output / name).write_bytes(payload)
        return ""

    def run_prepare(self, directory):
        template_path = Path(directory) / "template.rb.in"
        template_path.write_text(template(), encoding="utf-8")
        with patch.object(PREPARE, "run_gh", side_effect=self.mock_gh), \
                patch.object(GENERATE, "TEMPLATE", template_path):
            return PREPARE.prepare(self.tag, Path(directory) / "dist")

    def test_downloaded_bytes_are_verified_before_formula_written(self):
        with tempfile.TemporaryDirectory() as directory:
            output = self.run_prepare(directory)
            self.assertEqual(output.name, "gopls-mcp-proxy.rb")
            self.assertIn('version "0.7.0"', output.read_text(encoding="utf-8"))

    def test_absent_github_digest_still_requires_manifest_verification(self):
        for asset in self.release["assets"]:
            asset.pop("digest")
        with tempfile.TemporaryDirectory() as directory:
            self.run_prepare(directory)
        target = next(name for name in self.payloads if name.endswith(".tar.gz"))
        self.payloads[target] = b"X" * len(self.payloads[target])
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                self.run_prepare(directory)
            self.assertFalse((Path(directory) / "dist/gopls-mcp-proxy.rb").exists())

    def test_github_digest_and_asset_size_are_independently_checked(self):
        target = next(name for name in self.payloads if name.endswith(".tar.gz"))
        original = self.payloads[target]
        for payload, message in ((b"X" * len(original), "digest mismatch"), (b"short", "size")):
            self.payloads[target] = payload
            with tempfile.TemporaryDirectory() as directory:
                with self.assertRaisesRegex(ValueError, message):
                    self.run_prepare(directory)
                self.assertFalse((Path(directory) / "dist/gopls-mcp-proxy.rb").exists())

    def test_manifest_digest_is_also_checked(self):
        self.payloads["checksums.txt"] = self.payloads["checksums.txt"].replace(b"  ", b" *")
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, "digest mismatch: checksums.txt"):
                self.run_prepare(directory)

    def test_metadata_rejects_draft_prerelease_wrong_tag_and_bad_assets(self):
        invalid = []
        for field, value in (("draft", True), ("prerelease", True), ("tag_name", "v0.6.0")):
            invalid.append({**self.release, field: value})
        invalid.append({**self.release, "assets": self.release["assets"][:-1]})
        invalid.append({**self.release, "assets": self.release["assets"] * 2})
        for field, value in (("state", "new"), ("size", 0), ("digest", "md5:" + "a" * 32),
                             ("browser_download_url", "https://example.com/asset")):
            release = copy.deepcopy(self.release)
            release["assets"][0][field] = value
            invalid.append(release)
        for release in invalid:
            with self.subTest(release=release), self.assertRaises(ValueError):
                PREPARE.validate_release(self.tag, release)

    def test_invalid_release_never_starts_download_or_creates_output(self):
        self.release["prerelease"] = True
        with tempfile.TemporaryDirectory() as directory, \
                patch.object(PREPARE, "run_gh", return_value=json.dumps(self.release)) as gh:
            output = Path(directory) / "new"
            with self.assertRaises(ValueError):
                PREPARE.prepare(self.tag, output)
            self.assertEqual(gh.call_count, 1)
            self.assertFalse(output.exists())

    def test_nonempty_destination_is_never_overwritten(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(PREPARE, "run_gh") as gh:
            marker = Path(directory) / "keep"
            marker.write_text("existing", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "absent or empty"):
                PREPARE.prepare(self.tag, directory)
            gh.assert_not_called()
            self.assertEqual(marker.read_text(encoding="utf-8"), "existing")


if __name__ == "__main__":
    unittest.main()
