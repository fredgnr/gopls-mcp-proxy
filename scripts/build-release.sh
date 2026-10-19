#!/usr/bin/env bash
# Run on Linux with Go, GNU tar, gzip and sha256sum available.
set -euo pipefail
umask 022

cd "$(dirname "$0")/.."
version="${1:?usage: bash scripts/build-release.sh VERSION}"
numeric='(0|[1-9][0-9]*)'
identifier='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
semver="^v${numeric}\.${numeric}\.${numeric}(-${identifier}(\.${identifier})*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$"
if [[ ! "$version" =~ $semver && ! "$version" =~ ^dev-[0-9a-f]{12}$ ]]; then
  echo "Expected vMAJOR.MINOR.PATCH[-PRERELEASE][+BUILD] or dev-<12 hex digits>: $version" >&2
  exit 1
fi
commit="$(git rev-parse HEAD)"
epoch="$(git show -s --format=%ct HEAD)"
mkdir -p dist
shopt -s nullglob dotglob
existing=(dist/*)
if (( ${#existing[@]} != 0 )); then
  echo 'dist must be empty before building release packages' >&2
  exit 1
fi
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
package='github.com/fredgnr/gopls-mcp-proxy/internal/buildinfo'
ldflags="-s -w -X ${package}.Version=${version} -X ${package}.Commit=${commit}"
archives=()

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${target%/*}"
  arch="${target#*/}"
  name="gopls-mcp-proxy_${version}_${os}_${arch}"
  mkdir -p "$stage/$name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOAMD64=v1 GOARM64=v8.0 \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags "$ldflags" \
    -o "$stage/$name/gopls-mcp-proxy" ./cmd/gopls-mcp-proxy
  cp README.md "$stage/$name/README.md"
  printf 'version=%s\ncommit=%s\ntarget=%s\ngo=%s\n' \
    "$version" "$commit" "$target" "$(go env GOVERSION)" > "$stage/$name/BUILDINFO"

  # Exercise the packaged executable on the build host; other targets are cross-built.
  if [[ "$target" == "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" ]]; then
    "$stage/$name/gopls-mcp-proxy" --version > "$stage/stdout" 2> "$stage/stderr"
    test ! -s "$stage/stdout"
    test "$(cat "$stage/stderr")" = "gopls-mcp-proxy $version (commit $commit)"
    "$stage/$name/gopls-mcp-proxy" --help > "$stage/stdout" 2> "$stage/help"
    test ! -s "$stage/stdout"
  fi

  # Normalize archive metadata and omit gzip timestamps for reproducible packages.
  tar --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner \
    -C "$stage" -cf - "$name" | gzip -n > "dist/$name.tar.gz"
  archives+=("$name.tar.gz")
done

(
  cd dist
  sha256sum "${archives[@]}" > checksums.txt
  sha256sum --check checksums.txt
)
