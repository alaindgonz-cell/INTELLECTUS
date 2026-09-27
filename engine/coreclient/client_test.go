package coreclient_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/internal/fakecore"
)

// Many goroutines issue calls concurrently; the fake core answers in
// REVERSED batches. Every caller must get its own answer (demux by id) and
// every request line must parse (writes never interleave).
func TestClientDemuxAndSerializedWrites(t *testing.T) {
	srv := fakecore.New()
	srv.ReverseBatch = 7
	srv.Handle("echo", func(args json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]json.RawMessage{"echo": args}, nil
	})
	c := srv.Start()
	defer c.Close()

	const goroutines, perG = 48, 25
	big := make([]byte, 20000) // large payloads make torn writes likely if unserialized
	for i := range big {
		big[i] = 'a' + byte(i%26)
	}
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perG)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				want := fmt.Sprintf("g%d-i%d", g, i)
				var out struct {
					Echo struct {
						Tag string `json:"tag"`
						Pad string `json:"pad"`
					} `json:"echo"`
				}
				err := c.Call(context.Background(), "echo", map[string]string{"tag": want, "pad": string(big[:(g*37+i*11)%len(big)])}, &out)
				if err != nil {
					errs <- err
					return
				}
				if out.Echo.Tag != want {
					errs <- fmt.Errorf("got %q want %q", out.Echo.Tag, want)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := srv.ParseErrors(); n != 0 {
		t.Fatalf("%d request lines failed to parse (interleaved writes?)", n)
	}
	if got := srv.Count("echo"); got != goroutines*perG {
		t.Fatalf("server saw %d requests, want %d", got, goroutines*perG)
	}
}

func TestClientTransportErrorAndCancelledNotSent(t *testing.T) {
	srv := fakecore.New()
	srv.Handle("bad", func(json.RawMessage) (any, *coreclient.CoreError) {
		return nil, &coreclient.CoreError{Code: "BAD_ARGS", Message: "nope"}
	})
	c := srv.Start()
	defer c.Close()

	_, err := c.CallRaw(context.Background(), "bad", nil)
	if !coreclient.IsCoreError(err, "BAD_ARGS") {
		t.Fatalf("want BAD_ARGS core error, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CallRaw(ctx, "bad", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if n := srv.Count("bad"); n != 1 {
		t.Fatalf("cancelled call must not reach the core; core saw %d", n)
	}
}

func TestClientFailsPendingOnEOF(t *testing.T) {
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	c := coreclient.NewClient(respR, reqW)
	go func() { // read one request, then "crash"
		buf := make([]byte, 4096)
		_, _ = reqR.Read(buf)
		respW.Close()
	}()
	_, err := c.CallRaw(context.Background(), "hello", nil)
	if err == nil {
		t.Fatal("expected error after core EOF")
	}
	if !errors.Is(err, coreclient.ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
	if _, err := c.CallRaw(context.Background(), "hello", nil); err == nil {
		t.Fatal("calls after close must fail")
	}
}

func TestClientFailsClosedOnUnknownID(t *testing.T) {
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	c := coreclient.NewClient(respR, reqW)
	go func() {
		buf := make([]byte, 4096)
		_, _ = reqR.Read(buf)
		_, _ = respW.Write([]byte(`{"id":999,"ok":true,"result":{}}` + "\n"))
	}()
	if _, err := c.CallRaw(context.Background(), "hello", nil); err == nil {
		t.Fatal("expected failure on desynchronised response id")
	}
}

func TestIDRoundTrip(t *testing.T) {
	for _, tok := range []string{`"prop:9"`, `17`} {
		var id coreclient.ID
		if err := json.Unmarshal([]byte(tok), &id); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(id)
		if err != nil || string(b) != tok {
			t.Fatalf("round trip %s -> %s (%v)", tok, b, err)
		}
	}
	if coreclient.StringID("prop:9").String() != "prop:9" {
		t.Fatal("StringID/String mismatch")
	}
	if _, err := json.Marshal(coreclient.ID("prop:9")); err == nil {
		t.Fatal("bare non-JSON ID must not marshal")
	}
}

func TestOperatorSigning(t *testing.T) {
	seed, pubHex, err := coreclient.GenerateOperatorSeed()
	if err != nil {
		t.Fatal(err)
	}
	op, err := coreclient.NewOperator(seed, "proj:x")
	if err != nil {
		t.Fatal(err)
	}
	if op.KeyID() != pubHex {
		t.Fatalf("key id %s != %s", op.KeyID(), pubHex)
	}
	op.Now = func() time.Time { return time.Unix(1700000000, 0) }
	env, body, err := op.Envelope("add_requirement", map[string]any{
		"requirement_id": "R1", "text": "1 <= n & n <= 100", "sensitivity": "internal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if env.Body != string(body) {
		t.Fatal("transmitted body differs from signed body")
	}
	pub, _ := hex.DecodeString(env.KeyID)
	sig, _ := hex.DecodeString(env.Signature)
	msg := append([]byte("intellectus/v1/operator\n"), []byte(env.Body)...)
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		t.Fatal("signature does not verify over prefix+body")
	}
	if ed25519.Verify(ed25519.PublicKey(pub), []byte(env.Body), sig) {
		t.Fatal("signature must not verify without the domain prefix")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(env.Body), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["op"] != "add_requirement" || parsed["project_id"] != "proj:x" || parsed["issued_at"] != float64(1700000000) || parsed["nonce"] == "" || parsed["text"] != "1 <= n & n <= 100" {
		t.Fatalf("unexpected body %s", env.Body)
	}
	env2, _, _ := op.Envelope("shutdown", nil)
	var p2 map[string]any
	_ = json.Unmarshal([]byte(env2.Body), &p2)
	if p2["nonce"] == parsed["nonce"] {
		t.Fatal("nonces must be unique")
	}
	if err := coreclient.VerifyEnvelope(env); err != nil {
		t.Fatal(err)
	}
	env.Body += " "
	if coreclient.VerifyEnvelope(env) == nil {
		t.Fatal("tampered body must not verify")
	}
	if _, _, err := op.Envelope("x", map[string]any{"nonce": "replay"}); err == nil {
		t.Fatal("reserved param must be refused")
	}
	// The operator command carries the envelope object verbatim.
	srv := fakecore.New()
	srv.Handle("operator", func(args json.RawMessage) (any, *coreclient.CoreError) {
		e := fakecore.Arg[coreclient.Envelope](args, "envelope")
		if coreclient.VerifyEnvelope(e) != nil {
			return map[string]any{"status": "REJECTED", "reasons": []any{map[string]any{"code": "BAD_SIGNATURE"}}}, nil
		}
		return map[string]any{"status": "APPLIED", "event_sequence": 3, "result": map[string]any{"ref": "req:R1@1"}}, nil
	})
	c := srv.Start()
	defer c.Close()
	res, err := c.OperatorOp(context.Background(), op, "add_requirement", map[string]any{"requirement_id": "R1", "text": "t", "sensitivity": "internal"})
	if err != nil || res.Status != "APPLIED" || res.Ref() != "req:R1@1" {
		t.Fatalf("operator op: %+v %v", res, err)
	}
}

func TestReasonDecoding(t *testing.T) {
	var r struct {
		Reasons coreclient.Reasons `json:"reasons"`
	}
	in := `{"reasons":[{"code":"MISSING_APPROVAL","failed_check":null,"subject_reference":"prop:3","missing_dependency":"approval","counterexample_reference":null,"permitted_next_steps":["ESCALATE"],"remaining_budget":{"repairs":1}},"PLAIN_CODE"]}`
	if err := json.Unmarshal([]byte(in), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Reasons.Has("MISSING_APPROVAL") || !r.Reasons.Has("PLAIN_CODE") || r.Reasons[0].RemainingBudget["repairs"] != 1 {
		t.Fatalf("decoded %+v", r.Reasons)
	}
}
