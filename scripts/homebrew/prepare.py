#!/usr/bin/env python3
"""Download and verify an immutable published release before generating its formula."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess

from generate import REPOSITORY, archive_names, generate, read_checksums, stable_version


def run_gh(*args, timeout=120):
    return subprocess.run(["gh", *args], check=True, text=True, capture_output=True,
                          timeout=timeout).stdout


def validate_release(tag, release):
    stable_version(tag)
    if (release.get("tag_name") != tag or release.get("draft") is not False
            or release.get("prerelease") is not False):
        raise ValueError("Expected the requested published, non-prerelease GitHub Release")
    expected = {"checksums.txt", *archive_names(tag).values()}
    assets = {}
    for asset in release.get("assets", []):
        name = asset.get("name")
        if name not in expected:
            continue
        if name in assets:
            raise ValueError(f"Duplicate release asset: {name}")
        url = f"https://github.com/{REPOSITORY}/releases/download/{tag}/{name}"
        if (asset.get("state") != "uploaded" or not isinstance(asset.get("size"), int)
                or asset["size"] <= 0 or asset.get("browser_download_url") != url):
            raise ValueError(f"Release asset is not ready or has an unexpected URL: {name}")
        digest = asset.get("digest")
        if digest is not None and not re.fullmatch(r"sha256:[0-9a-fA-F]{64}", digest):
            raise ValueError(f"Invalid release asset digest: {name}")
        assets[name] = asset
    if set(assets) != expected:
        raise ValueError("Published release must include checksums.txt and all four platform archives")
    return assets


def file_sha256(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def prepare(tag, output_dir):
    stable_version(tag)
    output_dir = Path(output_dir)
    if output_dir.exists() and (not output_dir.is_dir() or any(output_dir.iterdir())):
        raise ValueError("Output directory must be absent or empty; use a fresh directory for each retry")
    release = json.loads(run_gh("api", f"repos/{REPOSITORY}/releases/tags/{tag}"))
    assets = validate_release(tag, release)
    output_dir.mkdir(parents=True, exist_ok=True)
    args = ["release", "download", tag, "--repo", REPOSITORY, "--dir", str(output_dir)]
    for name in sorted(assets):
        args.extend(["--pattern", name])
    run_gh(*args, timeout=300)
    if {path.name for path in output_dir.iterdir()} != set(assets):
        raise ValueError("Downloaded files do not match the expected release assets")
    actual = {}
    for name, metadata in assets.items():
        path = output_dir / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size != metadata["size"]:
            raise ValueError(f"Downloaded asset has an unexpected type or size: {name}")
        actual[name] = file_sha256(path)
        if metadata.get("digest") and actual[name] != metadata["digest"][7:].lower():
            raise ValueError(f"GitHub asset digest mismatch: {name}")
    manifest = output_dir / "checksums.txt"
    checksums = read_checksums(tag, manifest.read_text(encoding="utf-8"))
    for target, name in archive_names(tag).items():
        if actual[name] != checksums[target]:
            raise ValueError(f"Release checksum mismatch: {name}")
    return generate(tag, manifest, output_dir / "gopls-mcp-proxy.rb")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    args = parser.parse_args()
    try:
        output = prepare(args.tag, args.output_dir)
    except subprocess.CalledProcessError as error:
        # Do not echo arbitrary API/CLI stderr into Actions logs.
        parser.exit(1, f"Homebrew preparation failed: gh exited with status {error.returncode}\n")
    except (OSError, ValueError, subprocess.TimeoutExpired) as error:
        parser.exit(1, f"Homebrew preparation failed: {error}\n")
    print(f"Verified release {args.tag}; generated {output}")


if __name__ == "__main__":
    main()
