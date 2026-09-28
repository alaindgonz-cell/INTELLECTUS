package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDemoAgainstRealCore runs the full workflow against a real
// intellectus-core binary. It is skipped unless INTELLECTUS_CORE points at
// the binary (unit tests never depend on the Rust build).
func TestDemoAgainstRealCore(t *testing.T) {
	bin := os.Getenv("INTELLECTUS_CORE")
	if bin == "" {
		t.Skip("set INTELLECTUS_CORE=/path/to/intellectus-core to run the end-to-end demo")
	}
	var out bytes.Buffer
	d := &demo{core: bin, deductor: "rust", workdir: t.TempDir(), out: &out}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := d.run(ctx); err != nil {
		t.Fatalf("demo failed: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"core=FAIL", "applied_choice=REPAIR fallback=\"SHADOW_MODE\"", "core=PASS",
		"[MISSING_APPROVAL]", "AUTHORIZED", "ARTIFACT_BINDING_MISMATCH", "IDEMPOTENCY_CONFLICT",
		"record_outcome_unknown -> OUTCOME_UNKNOWN", "effect_observed=true", "adapter Execute calls = 1",
		"-> COMPLETED", "matches=true",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("demo output lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(out.String())
	}
}
