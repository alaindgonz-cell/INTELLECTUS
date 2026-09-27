package advisor_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/advisor"
	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
	"github.com/alaindgonz-cell/intellectus/engine/internal/fakecore"
)

// routingCore mimics the core's §4 rule: used_advisor only for an eligible
// choice with mode LIVE and confidence >= 8000; otherwise the first
// eligible of REPAIR, ESCALATE, STOP.
func routingCore(t *testing.T, eligible []string, det *string, lieUsedAdvisor bool) (*fakecore.Server, *coreclient.Client) {
	t.Helper()
	srv := fakecore.New()
	srv.Handle("route", func(json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"eligible": eligible, "deterministic_choice": det, "reason": "test"}, nil
	})
	srv.Handle("view", func(json.RawMessage) (any, *coreclient.CoreError) {
		return map[string]any{"snapshot_sequence": 5, "state_digest": "sha256:view", "policy_version": "P1"}, nil
	})
	srv.Handle("record_assessment", func(args json.RawMessage) (any, *coreclient.CoreError) {
		var a coreclient.AssessmentArgs
		_ = json.Unmarshal(args, &a)
		if a.Choice != nil && !slices.Contains(coreclient.RouteOptions, *a.Choice) {
			return nil, &coreclient.CoreError{Code: "BAD_ARGS", Message: "unknown option"}
		}
		if a.Choice != nil && slices.Contains(eligible, *a.Choice) && a.Mode == "LIVE" && a.ConfidenceBP >= 8000 {
			return map[string]any{"assessment_id": "asm:1", "applied_choice": *a.Choice, "used_advisor": true}, nil
		}
		for _, o := range []string{"REPAIR", "ESCALATE", "STOP"} {
			if slices.Contains(eligible, o) {
				return map[string]any{"assessment_id": "asm:1", "applied_choice": o, "used_advisor": lieUsedAdvisor}, nil
			}
		}
		return nil, &coreclient.CoreError{Code: "BAD_ARGS", Message: "no fallback"}
	})
	c := srv.Start()
	t.Cleanup(func() { c.Close() })
	return srv, c
}

func lastAssessment(t *testing.T, srv *fakecore.Server) coreclient.AssessmentArgs {
	t.Helper()
	reqs := srv.Requests("record_assessment")
	if len(reqs) == 0 {
		t.Fatal("no record_assessment")
	}
	var a coreclient.AssessmentArgs
	if err := json.Unmarshal(reqs[len(reqs)-1].Args, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

var eligible = []string{"GATHER_CONTEXT", "REPAIR", "ESCALATE"}

func gatherAdvice() advisor.Advice {
	return advisor.Advice{Choice: "GATHER_CONTEXT", ConfidenceBP: 9100,
		ProbabilitiesBP: map[string]int64{"GATHER_CONTEXT": 9100, "REPAIR": 700, "ESCALATE": 200}}
}

func TestDeterministicChoiceSkipsAdvisor(t *testing.T) {
	det := "ESCALATE"
	srv, c := routingCore(t, []string{"ESCALATE", "STOP"}, &det, false)
	adv := &advisor.SimulatedAdvisor{Script: []advisor.Advice{gatherAdvice()}}
	sr := &advisor.ShadowRunner{Core: c, Session: coreclient.StringID("adv"), Advisor: adv, Mode: advisor.ModeShadow}
	d, err := sr.Decide(context.Background(), "task:x", []string{"acceptance_tests:FAIL"})
	if err != nil || d.Applied != "ESCALATE" || d.Source != "deterministic" {
		t.Fatalf("%+v %v", d, err)
	}
	if adv.Calls() != 0 || srv.Count("record_assessment") != 0 {
		t.Fatal("advisor must not be consulted when a deterministic choice exists")
	}
}

func TestShadowModeRecordsButAppliesCoreFallback(t *testing.T) {
	srv, c := routingCore(t, eligible, nil, false)
	adv := &advisor.SimulatedAdvisor{Script: []advisor.Advice{gatherAdvice()}}
	sr := &advisor.ShadowRunner{Core: c, Session: coreclient.StringID("adv"), Advisor: adv, Mode: advisor.ModeShadow, Timeout: time.Second}
	d, err := sr.Decide(context.Background(), "task:x", []string{"acceptance_tests:FAIL"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Applied != "REPAIR" || d.Source != "core_fallback" || d.Advice.Choice != "GATHER_CONTEXT" {
		t.Fatalf("shadow must apply the core's fallback, got %+v", d)
	}
	a := lastAssessment(t, srv)
	if a.Choice == nil || *a.Choice != "GATHER_CONTEXT" || a.ConfidenceBP != 9100 || a.Mode != "SIMULATED" ||
		a.FallbackReason == nil || *a.FallbackReason != advisor.FallbackShadow || a.ProbabilitiesBP["GATHER_CONTEXT"] != 9100 ||
		a.InputManifestDigest != "sha256:view" || a.RoutingPolicyVersion != "P1" || !slices.Equal(a.Eligible, eligible) {
		t.Fatalf("assessment args %+v", a)
	}
}

func TestShadowRefusesCoreThatLetsAdvisorSteer(t *testing.T) {
	_, c := routingCore(t, eligible, nil, true)
	sr := &advisor.ShadowRunner{Core: c, Session: coreclient.StringID("adv"),
		Advisor: &advisor.SimulatedAdvisor{Script: []advisor.Advice{gatherAdvice()}}, Mode: advisor.ModeShadow}
	if _, err := sr.Decide(context.Background(), "task:x", nil); err == nil {
		t.Fatal("used_advisor=true in SHADOW must fail closed")
	}
}

func TestAdvisorTimeoutFallsBack(t *testing.T) {
	srv, c := routingCore(t, eligible, nil, false)
	adv := &advisor.SimulatedAdvisor{Script: []advisor.Advice{gatherAdvice()}, Delay: 2 * time.Second}
	sr := &advisor.ShadowRunner{Core: c, Session: coreclient.StringID("adv"), Advisor: adv, Mode: advisor.ModeLive, Timeout: 20 * time.Millisecond}
	start := time.Now()
	d, err := sr.Decide(context.Background(), "task:x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("timeout not enforced")
	}
	a := lastAssessment(t, srv)
	if d.Applied != "REPAIR" || a.FallbackReason == nil || *a.FallbackReason != advisor.FallbackTimeout || a.Choice != nil {
		t.Fatalf("decision %+v args %+v", d, a)
	}
}

// An advisor that ignores its context cannot stall routing.
type stuckAdvisor struct{ release chan struct{} }

func (s stuckAdvisor) Advise(context.Context, advisor.Question) (advisor.Advice, error) {
	<-s.release
	return advisor.Advice{}, nil
}

func TestAdvisorIgnoringContextStillTimesOut(t *testing.T) {
	_, c := routingCore(t, eligible, nil, false)
	st := stuckAdvisor{release: make(chan struct{})}
	defer close(st.release)
	sr := &advisor.ShadowRunner{Core: c, Session: coreclient.StringID("adv"), Advisor: st, Mode: advisor.ModeLive, Timeout: 20 * time.Millisecond}
	d, err := sr.Decide(context.Background(), "task:x", nil)
	if err != nil || d.FallbackReason != advisor.FallbackTimeout || d.Applied != "REPAIR" {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestOutOfEligibleChoiceFallsBack(t *testing.T) {
	srv, c := routingCore(t, eligible, nil, false)
	// Even in LIVE mode with a LIVE-labelled advisor and high confidence.
	bad := advisor.Advice{Choice: "STOP", ConfidenceBP: 9900, Mode: advisor.ModeLive, ProbabilitiesBP: map[string]int64{"STOP": 10000}}
	sr := &advisor.ShadowRunner{Core: c, Session: coreclient.StringID("adv"), Advisor: &advisor.SimulatedAdvisor{Script: []advisor.Advice{bad}}, Mode: advisor.ModeLive}
	d, err := sr.Decide(context.Background(), "task:x", nil)
	if err != nil || d.Applied != "REPAIR" || d.Source != "core_fallback" || !strings.HasPrefix(d.FallbackReason, advisor.FallbackNotEligible) {
		t.Fatalf("%+v %v", d, err)
	}
	if a := lastAssessment(t, srv); a.FallbackReason == nil {
		t.Fatal("fallback reason must be recorded")
	}
	// A non-protocol option is never forwarded as a choice.
	weird := advisor.Advice{Choice: "MARK_COMPLETE", ConfidenceBP: 10000, Mode: advisor.ModeLive}
	sr.Advisor = &advisor.SimulatedAdvisor{Script: []advisor.Advice{weird}}
	d, err = sr.Decide(context.Background(), "task:x", nil)
	if err != nil || d.Applied != "REPAIR" {
		t.Fatalf("%+v %v", d, err)
	}
	if a := lastAssessment(t, srv); a.Choice != nil {
		t.Fatal("non-option choice must not be forwarded")
	}
}

func TestMalformedAndErrorFallBack(t *testing.T) {
	srv, c := routingCore(t, eligible, nil, false)
	mal := advisor.Advice{Choice: "REPAIR", ConfidenceBP: 9000, ProbabilitiesBP: map[string]int64{"REPAIR": 6000}}
	sr := &advisor.ShadowRunner{Core: c, Session: coreclient.StringID("adv"), Advisor: &advisor.SimulatedAdvisor{Script: []advisor.Advice{mal}}, Mode: advisor.ModeLive}
	d, err := sr.Decide(context.Background(), "task:x", nil)
	if err != nil || !strings.HasPrefix(d.FallbackReason, advisor.FallbackMalformed) || lastAssessment(t, srv).Choice != nil {
		t.Fatalf("%+v %v", d, err)
	}
	sr.Advisor = &advisor.SimulatedAdvisor{Err: errors.New("boom")}
	d, err = sr.Decide(context.Background(), "task:x", nil)
	if err != nil || !strings.HasPrefix(d.FallbackReason, advisor.FallbackError) || d.Applied != "REPAIR" {
		t.Fatalf("%+v %v", d, err)
	}
	// Simulated advice in LIVE routing mode is still not LIVE: fallback.
	sr.Advisor = &advisor.SimulatedAdvisor{Script: []advisor.Advice{gatherAdvice()}}
	d, err = sr.Decide(context.Background(), "task:x", nil)
	if err != nil || d.Applied != "REPAIR" || !strings.HasPrefix(d.FallbackReason, advisor.FallbackNotLive) {
		t.Fatalf("%+v %v", d, err)
	}
}
