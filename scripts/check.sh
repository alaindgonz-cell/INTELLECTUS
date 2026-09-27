#!/usr/bin/env bash
# Run every component's checks. Mojo steps are skipped (loudly) when no Mojo
# toolchain is available; the Rust reference still covers the semantics.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"

echo "== Mojo deductor (build + conformance)"
if command -v "${MOJO:-mojo}" >/dev/null 2>&1; then
  MOJO="${MOJO:-mojo}" "$root/deductor/build.sh"
  "$root/conformance/deduce/run.sh" "$root/deductor/build/deductor"
else
  echo "SKIPPED: no Mojo toolchain (set MOJO=/path/to/mojo)"
fi

echo "== Rust core (fmt, clippy, tests incl. Rust<->Mojo differential when built)"
(cd "$root/core" && cargo fmt --check && cargo clippy --all-targets -- -D warnings && cargo test)

echo "== Go engine (vet, race tests)"
(cd "$root/engine" && go vet ./... && go test -race ./...)

echo "== End-to-end demo (real core binary, fake worker, simulated advisor)"
(cd "$root/core" && cargo build --quiet)
work="$(mktemp -d)"
deductor="rust"
[ -x "$root/deductor/build/deductor" ] && deductor="cross:$root/deductor/build/deductor"
(cd "$root/engine" && go run ./cmd/intellectus-demo --core "$root/core/target/debug/intellectus-core" --deductor "$deductor" --workdir "$work")
echo "ALL CHECKS PASSED"
