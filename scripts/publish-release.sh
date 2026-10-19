#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
: "${GH_TOKEN:?GH_TOKEN is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_SHA:?GITHUB_SHA is required}"
tag="${GITHUB_REF_NAME:?GITHUB_REF_NAME is required}"
if [[ "${GITHUB_EVENT_NAME:-}" != push || "${GITHUB_REF_TYPE:-}" != tag ]]; then
  echo 'Publishing requires a tag push event' >&2
  exit 1
fi

# Resolve lightweight and annotated tags; never release a moved tag or create one.
object="$(gh api "repos/$GITHUB_REPOSITORY/git/ref/tags/$tag" --jq '.object | [.type, .sha] | @tsv')"
read -r type sha <<< "$object"
while [[ "$type" == tag ]]; do
  object="$(gh api "repos/$GITHUB_REPOSITORY/git/tags/$sha" --jq '.object | [.type, .sha] | @tsv')"
  read -r type sha <<< "$object"
done
if [[ "$type" != commit || "$sha" != "$GITHUB_SHA" ]]; then
  echo 'The remote tag no longer points to the commit validated by this run' >&2
  exit 1
fi

archives=()
for target in linux_amd64 linux_arm64 darwin_amd64 darwin_arm64; do
  archives+=("gopls-mcp-proxy_${tag}_${target}.tar.gz")
done
assets=(dist/checksums.txt "${archives[@]/#/dist/}")
for asset in "${assets[@]}"; do test -s "$asset"; done
expected="$(cd dist && sha256sum "${archives[@]}")"
if [[ "$(cat dist/checksums.txt)" != "$expected" ]]; then
  echo 'The checksum manifest must match all four release archives' >&2
  exit 1
fi

prerelease=false
if [[ "${tag%%+*}" == *-* ]]; then prerelease=true; fi
if draft="$(gh release view "$tag" --repo "$GITHUB_REPOSITORY" --json isDraft --jq .isDraft 2>/dev/null)"; then
  if [[ "$draft" != true ]]; then
    echo 'This release is already published; refusing to replace its assets' >&2
    exit 1
  fi
else
  gh release create "$tag" --repo "$GITHUB_REPOSITORY" --verify-tag \
    --title "$tag" --generate-notes --draft --prerelease="$prerelease"
fi

# Failed uploads leave a draft that a rerun can complete. Published assets stay untouched.
gh release upload "$tag" "${assets[@]}" --repo "$GITHUB_REPOSITORY" --clobber
gh release edit "$tag" --repo "$GITHUB_REPOSITORY" --draft=false --prerelease="$prerelease"
