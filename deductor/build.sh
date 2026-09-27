#!/usr/bin/env bash
# Build the Mojo deductor to deductor/build/deductor.
# Uses $MOJO if set, otherwise `mojo` from PATH.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MOJO="${MOJO:-mojo}"
if ! command -v "$MOJO" > /dev/null 2>&1; then
    echo "build.sh: mojo compiler not found (set MOJO=/path/to/mojo or put mojo on PATH)" >&2
    exit 1
fi
mkdir -p "$HERE/build"
"$MOJO" build "$HERE/deductor.mojo" -o "$HERE/build/deductor"
echo "built $HERE/build/deductor ($("$MOJO" --version 2>/dev/null | head -n 1))"
