#!/usr/bin/env bash
# Package cross-built binaries into archives with checksums.
set -euo pipefail

DIST="${1:-dist}"
BINARY="${2:-boop}"
VERSION="${3:-0.0.0}"

[ -d "$DIST" ] || { echo "no $DIST directory; run 'make build-all' first" >&2; exit 1; }

cd "$DIST"
rm -f checksums.txt
for dir in "${BINARY}_${VERSION}"_*/; do
    [ -d "$dir" ] || continue
    name="${dir%/}"
    case "$name" in
        *_windows_*) zip -qr "${name}.zip" "$name" && echo "packaged ${name}.zip" ;;
        *)           tar -czf "${name}.tar.gz" "$name" && echo "packaged ${name}.tar.gz" ;;
    esac
done

shopt -s nullglob
archives=(*.tar.gz *.zip)
if [ ${#archives[@]} -gt 0 ]; then
    sha256sum "${archives[@]}" > checksums.txt
    echo "wrote $DIST/checksums.txt"
fi
