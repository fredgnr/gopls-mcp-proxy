#!/usr/bin/env python3
"""Render the binary Homebrew formula from a complete release checksum manifest."""

import argparse
from pathlib import Path
import re


REPOSITORY = "fredgnr/gopls-mcp-proxy"
TARGETS = ("darwin_arm64", "darwin_amd64", "linux_arm64", "linux_amd64")
STABLE_TAG = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
CHECKSUM = re.compile(r"([0-9a-fA-F]{64}) [ *](\S+)")
TEMPLATE = Path(__file__).with_name("gopls-mcp-proxy.rb.in")
# Reinstall the original v0.7.0 package without its former managed toolchain.
# New upstream versions start at Homebrew's default revision of zero.
PACKAGING_REVISIONS = {"v0.7.0": 1}


def stable_version(tag):
    match = STABLE_TAG.fullmatch(tag)
    if not match:
        raise ValueError("Homebrew stable releases require vMAJOR.MINOR.PATCH (no prerelease/build suffix)")
    return tuple(int(part) for part in match.groups())


def archive_names(tag):
    stable_version(tag)
    return {target: f"gopls-mcp-proxy_{tag}_{target}.tar.gz" for target in TARGETS}


def read_checksums(tag, text):
    expected = archive_names(tag)
    checksums = {}
    for line in text.splitlines():
        match = CHECKSUM.fullmatch(line)
        if not match:
            raise ValueError("Malformed SHA-256 manifest line")
        checksum, filename = match.groups()
        if filename in checksums:
            raise ValueError(f"Duplicate checksum entry: {filename}")
        if filename not in expected.values():
            raise ValueError(f"Unexpected checksum entry: {filename}")
        checksums[filename] = checksum.lower()
    if set(checksums) != set(expected.values()):
        raise ValueError("Checksum manifest must contain exactly all four platform archives")
    return {target: checksums[filename] for target, filename in expected.items()}


def render(tag, checksums, template=None):
    stable_version(tag)
    if set(checksums) != set(TARGETS) or any(
        not re.fullmatch(r"[0-9a-f]{64}", value) for value in checksums.values()
    ):
        raise ValueError("Formula requires four valid SHA-256 values")
    source = TEMPLATE.read_text(encoding="utf-8") if template is None else template
    values = {"VERSION": tag[1:], "TAG": tag}
    values.update({f"SHA256_{target.upper()}": checksum for target, checksum in checksums.items()})
    required = {"VERSION", "TAG", "REVISION", *(f"SHA256_{target.upper()}" for target in TARGETS)}
    seen = set(re.findall(r"@([A-Z0-9_]+)@", source))
    if seen != required:
        raise ValueError("Formula template has missing or unknown placeholders")
    revision = PACKAGING_REVISIONS.get(tag, 0)
    if len(re.findall(r"^@REVISION@$", source, re.MULTILINE)) != 1 or source.count("@REVISION@") != 1:
        raise ValueError("Formula template must have one standalone revision placeholder")
    source = source.replace("@REVISION@\n", f"  revision {revision}\n" if revision else "")
    return re.sub(r"@([A-Z0-9_]+)@", lambda match: values[match.group(1)], source)


def guard_current(tag, checksums, current):
    """Refuse rollback and mutation of hashes for an already published version."""
    # Homebrew derives the version from the archive URL. Read the four URLs as
    # one release identity instead of depending on a redundant version stanza.
    pairs = re.findall(r'^\s*url "([^"]+)"\s*\n\s*sha256 "([0-9a-fA-F]{64})"\s*$',
                       current, re.MULTILINE)
    if len(pairs) != len(TARGETS) or len(re.findall(r'^\s*url\b', current, re.MULTILINE)) != len(TARGETS):
        raise ValueError("Current formula must contain exactly four release URLs and checksums")
    release_url = re.compile(
        re.escape(f"https://github.com/{REPOSITORY}/releases/download/")
        + r"(?P<tag>v[0-9]+\.[0-9]+\.[0-9]+)/gopls-mcp-proxy_(?P=tag)_"
        + r"(?P<target>" + "|".join(TARGETS) + r")\.tar\.gz"
    )
    previous_checksums = {}
    previous_tags = set()
    for url, checksum in pairs:
        match = release_url.fullmatch(url)
        if not match:
            raise ValueError("Cannot verify existing formula release URL")
        previous_tag, target = match.group("tag", "target")
        stable_version(previous_tag)
        if target in previous_checksums:
            raise ValueError("Current formula contains a duplicate release platform")
        previous_checksums[target] = checksum.lower()
        previous_tags.add(previous_tag)
    if len(previous_tags) != 1 or set(previous_checksums) != set(TARGETS):
        raise ValueError("Current formula URLs must identify the same release on all four platforms")
    previous_tag = previous_tags.pop()
    # Accept older generated formulas with a version stanza, but require it to
    # agree with the assets rather than allowing it to mask a rollback.
    versions = re.findall(r'^\s*version "([^"]+)"\s*$', current, re.MULTILINE)
    version_stanzas = len(re.findall(r'^\s*version\b', current, re.MULTILINE))
    if version_stanzas > 1 or version_stanzas != len(versions):
        raise ValueError("Current formula contains an invalid explicit version")
    if versions and f"v{versions[0]}" != previous_tag:
        raise ValueError("Current formula explicit version disagrees with release URLs")
    previous = stable_version(previous_tag)
    requested = stable_version(tag)
    if requested < previous:
        raise ValueError(f"Refusing Homebrew downgrade from {previous_tag} to {tag}")
    if requested != previous:
        return
    for target, checksum in previous_checksums.items():
        if checksum != checksums[target]:
            raise ValueError(f"Refusing changed release checksum for {tag}: {target}")


def generate(tag, manifest, output, current_formula=None):
    checksums = read_checksums(tag, Path(manifest).read_text(encoding="utf-8"))
    if current_formula is not None and Path(current_formula).exists():
        guard_current(tag, checksums, Path(current_formula).read_text(encoding="utf-8"))
    formula = render(tag, checksums)
    output = Path(output)
    output.parent.mkdir(parents=True, exist_ok=True)
    temporary = output.with_name(output.name + ".tmp")
    temporary.write_text(formula, encoding="utf-8")
    temporary.replace(output)
    return output


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--checksums", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--current-formula", type=Path)
    args = parser.parse_args()
    try:
        generate(args.tag, args.checksums, args.output, args.current_formula)
    except (OSError, ValueError) as error:
        parser.exit(1, f"Homebrew generation failed: {error}\n")


if __name__ == "__main__":
    main()
