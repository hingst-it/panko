#!/usr/bin/env bash
# Builds the release artifacts exactly as the Release workflow publishes them; candidate builds and
# the acceptance tests (test/release_acceptance.sh) use the same script. Run it in a clean checkout:
# it stamps the tag's version into charts/panko/Chart.yaml, which the CLI embeds.
#
#   tools/release-build.sh binaries TAG OUT   GOOS/GOARCH archives (panko, panko-admin; Linux also panko-server)
#   tools/release-build.sh chart TAG OUT      the packaged Helm chart, checked against the published images
#   tools/release-build.sh checksums OUT      SHA256SUMS over every archive and the chart
set -euo pipefail
# macOS tar would otherwise add AppleDouble files to local candidate archives.
export COPYFILE_DISABLE=1
absolute() { mkdir -p "$1" && (cd "$1" && pwd); }
sha256() { if command -v sha256sum > /dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }

command="${1:?usage: release-build.sh binaries|chart|checksums ...}"
case "$command" in
  binaries)
    tag="${2:?TAG}" out=$(absolute "${3:?OUT}")
    version=$(python3 tools/release.py stamp "$tag")
    goos="${GOOS:-$(go env GOOS)}" goarch="${GOARCH:-$(go env GOARCH)}"
    staging=$(mktemp -d)
    trap 'rm -rf "$staging"' EXIT
    mkdir -p "$staging/cli" "$staging/server"
    suffix=''
    if [ "$goos" = windows ]; then suffix='.exe'; fi
    export CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch"
    flags="-s -w -X main.version=$tag"
    go build -trimpath -ldflags="$flags" -o "$staging/cli/panko$suffix" ./cmd/panko
    go build -trimpath -ldflags="$flags" -o "$staging/cli/panko-admin$suffix" ./cmd/admin
    cp LICENSE README.md CHANGELOG.md "$staging/cli/"
    archive="panko_${version}_${goos}_${goarch}"
    if [ "$goos" = windows ]; then
      (cd "$staging/cli" && zip -q "$out/$archive.zip" ./*)
    else
      tar -czf "$out/$archive.tar.gz" -C "$staging/cli" .
    fi
    # Remote agents and the server roles run on Linux only.
    if [ "$goos" = linux ]; then
      go build -trimpath -ldflags="$flags" -o "$staging/server/panko-server" ./cmd/panko-server
      cp LICENSE README.md CHANGELOG.md "$staging/server/"
      cp deploy/agent/panko-agent.service deploy/agent/agent.env.example deploy/agent/profile.env.example "$staging/server/"
      tar -czf "$out/panko-server_${version}_linux_${goarch}.tar.gz" -C "$staging/server" .
    fi
    ;;
  chart)
    tag="${2:?TAG}" out=$(absolute "${3:?OUT}")
    version=$(python3 tools/release.py stamp "$tag")
    # File times of a checkout differ per run; the commit time keeps the package reproducible.
    epoch=$(git log -1 --format=%ct)
    python3 -c 'import os, sys; [os.utime(os.path.join(d, f), (int(sys.argv[2]),) * 2) for d, _, fs in os.walk(sys.argv[1]) for f in fs]' charts/panko "$epoch"
    helm package charts/panko --destination "$out" > /dev/null
    package="$out/panko-$version.tgz"
    rendered=$(mktemp)
    trap 'rm -f "$rendered"' EXIT
    # Render the packaged chart, not the source tree, with every optional image enabled.
    helm template panko "$package" --namespace panko-system --set installation.s3=true > "$rendered"
    python3 tools/release.py check "$version" "$rendered"
    ;;
  checksums)
    out=$(absolute "${2:?OUT}")
    (cd "$out" && shopt -s nullglob && files=(*.tar.gz *.zip *.tgz) && sha256 "${files[@]}" > SHA256SUMS && sha256 --check --quiet SHA256SUMS)
    ;;
  *)
    echo "unknown command $command" >&2
    exit 2
    ;;
esac
