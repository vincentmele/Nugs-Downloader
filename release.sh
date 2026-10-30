#!/usr/bin/env bash
# Builds nugs_dl for every supported platform into dist/, one archive each
# plus SHA256SUMS. With --publish, uploads them as a GitHub release (needs gh),
# using the given notes file or GitHub's generated notes.
#
#   ./release.sh v1.2.3
#   ./release.sh v1.2.3 --publish [notes.md]
set -euo pipefail

version="${1:-}"
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]]; then
  echo "usage: $0 vX.Y.Z [--publish [notes.md]]" >&2
  exit 2
fi
publish="${2:-}"
notes="${3:-}"
repo="vincentmele/Nugs-Downloader"
targets=(
  darwin/amd64 darwin/arm64
  linux/amd64 linux/arm64 linux/arm linux/386
  windows/amd64 windows/arm64 windows/386
  freebsd/amd64
  android/arm64
)

cd "$(dirname "$0")"
if [[ "$publish" == "--publish" ]]; then
  if [[ -n "$(git status --porcelain)" ]]; then
    echo "commit your changes before publishing" >&2
    exit 1
  fi
  if [[ -z "$(git branch -r --contains HEAD)" ]]; then
    echo "push this commit before publishing" >&2
    exit 1
  fi
fi

go vet ./...
go test ./...
rm -rf dist
mkdir -p dist
for target in "${targets[@]}"; do
  os="${target%/*}"
  arch="${target#*/}"
  name="nugs_dl_${version}_${os}_${arch}"
  bin="nugs_dl"
  [[ "$os" == windows ]] && bin="nugs_dl.exe"
  mkdir -p "dist/$name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOARM=7 \
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "dist/$name/$bin" .
  cp README.md token.md config.example.json .env.example "dist/$name/"
  if [[ "$os" == windows ]]; then
    (cd dist && zip -qr "$name.zip" "$name")
  else
    tar -C dist -czf "dist/$name.tar.gz" "$name"
  fi
  rm -rf "dist/$name"
  echo "built $name"
done
(cd dist && shasum -a 256 *.zip *.tar.gz > SHA256SUMS)

if [[ "$publish" == "--publish" ]]; then
  notesArgs=(--generate-notes)
  [[ -n "$notes" ]] && notesArgs=(--notes-file "$notes")
  gh release create "$version" dist/*.zip dist/*.tar.gz dist/SHA256SUMS \
    --repo "$repo" --target "$(git rev-parse HEAD)" --title "nugs_dl $version" "${notesArgs[@]}"
fi
