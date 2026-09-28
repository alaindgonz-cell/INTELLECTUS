package advisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/alaindgonz-cell/intellectus/engine/coreclient"
)

// Fallback reasons recorded with record_assessment.
const (
	FallbackDisabled    = "ADVISOR_DISABLED"
	FallbackTimeout     = "ADVISOR_TIMEOUT"
	FallbackError       = "ADVISOR_ERROR"
	FallbackMalformed   = "ADVISOR_MALFORMED"
	FallbackNotEligible = "CHOICE_NOT_ELIGIBLE"
	FallbackShadow      = "SHADOW_MODE"
	FallbackNotLive     = "ADVISOR_NOT_LIVE"
	FallbackNoView      = "VIEW_UNAVAILABLE"
)

// QuestionTemplate is the fixed question shape shown to an advisor.
const QuestionTemplate = "intellectus/jev-question/v1: given task {task_id}, failure codes {failure_codes} and the eligible route options {eligible}, choose exactly one eligible option; give integer basis-point probabilities per option summing to 10000 and an integer basis-point confidence."

// QuestionTemplateDigest identifies QuestionTemplate. It is provenance
// metadata about engine-owned text, not a digest the core relies on.
func QuestionTemplateDigest() string {
	h := sha256.Sum256([]byte("intellectus-engine/question-template\n" + QuestionTemplate))
	return "sha256:" + hex.EncodeToString(h[:])
}

// Decision is the routing outcome for one failure.
type Decision struct {
	Route *coreclient.RouteResult
	// Applied is the option the engine will follow (always core-issued).
	Applied string
	// Source: "deterministic" (route.deterministic_choice), "core_fallback"
	// (record_assessment applied the deterministic fallback), or "advisor"
	// (LIVE advisor accepted by the core).
	Source         string
	Advice         *Advice
	AdviceErr      error
	FallbackReason string
	Assessment     *coreclient.RecordAssessmentResult
}

// ShadowRunner consults a DecisionAdvisor around the core's route.
type ShadowRunner struct {
	Core *coreclient.Client
	// Session is the advisor session (role "advisor").
	Session coreclient.ID
	Advisor DecisionAdvisor // nil = disabled
	// Mode is SHADOW, LIVE or OFF (engine-side routing mode).
	Mode    string
	Timeout time.Duration
}

// Decide routes one failure.
func (s *ShadowRunner) Decide(ctx context.Context, taskID string, failureCodes []string) (*Decision, error) {
	route, err := s.Core.Route(ctx, taskID, failureCodes)
	if err != nil {
		return nil, fmt.Errorf("route: %w", err)
	}
	d := &Decision{Route: route}
	// Deterministic rules first: never ask the advisor.
	if route.DeterministicChoice != nil {
		if !slices.Contains(route.Eligible, *route.DeterministicChoice) && len(route.Eligible) > 0 {
			return nil, fmt.Errorf("route: deterministic choice %q not in eligible %v", *route.DeterministicChoice, route.Eligible)
		}
		d.Applied, d.Source = *route.DeterministicChoice, "deterministic"
		return d, nil
	}

	args := coreclient.AssessmentArgs{
		SessionID:              s.Session,
		TaskID:                 taskID,
		FailureCodes:           failureCodes,
		Eligible:               route.Eligible,
		QuestionTemplateDigest: QuestionTemplateDigest(),
		Mode:                   ModeOff,
	}
	fallback := ""
	switch {
	case s.Advisor == nil || s.Mode == ModeOff || s.Mode == "":
		fallback = FallbackDisabled
	default:
		view, verr := s.Core.View(ctx, s.Session, taskID)
		if verr != nil {
			fallback = FallbackNoView + ": " + verr.Error()
			break
		}
		args.InputManifestDigest = view.StateDigest
		args.RoutingPolicyVersion = view.PolicyVersion
		q := Question{TaskID: taskID, Eligible: slices.Clone(route.Eligible), FailureCodes: slices.Clone(failureCodes), View: view.Raw}
		actx, cancel := ctx, context.CancelFunc(func() {})
		if s.Timeout > 0 {
			actx, cancel = context.WithTimeout(ctx, s.Timeout)
		}
		start := time.Now()
		adv, aerr := safeAdvise(actx, s.Advisor, q)
		args.LatencyMS = time.Since(start).Milliseconds()
		timedOut := actx.Err() != nil && ctx.Err() == nil
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, err // operator cancellation: record nothing further
		}
		switch {
		case aerr != nil && (timedOut || errors.Is(aerr, context.DeadlineExceeded)):
			d.AdviceErr = aerr
			fallback = FallbackTimeout
		case aerr != nil:
			d.AdviceErr = aerr
			fallback = FallbackError + ": " + aerr.Error()
		default:
			d.Advice = &adv
			args.ModelRequested, args.ModelReturned = adv.ModelRequested, adv.ModelReturned
			args.Usage = adv.Usage
			args.ProviderResponseRef = adv.ProviderRef
			args.Mode = adv.Mode
			if s.Mode == ModeShadow && adv.Mode == ModeLive {
				args.Mode = ModeShadow
			}
			if msg := validate(adv); msg != "" {
				fallback = FallbackMalformed + ": " + msg
				break
			}
			// Well-formed: record what the advisor said.
			choice := adv.Choice
			args.ProbabilitiesBP = adv.ProbabilitiesBP
			args.ConfidenceBP = adv.ConfidenceBP
			switch {
			case !slices.Contains(route.Eligible, choice):
				fallback = FallbackNotEligible + ": " + choice
				if slices.Contains(coreclient.RouteOptions, choice) {
					args.Choice = &choice // the core re-validates and refuses it
				}
			case s.Mode == ModeShadow:
				args.Choice = &choice
				fallback = FallbackShadow
			case args.Mode != ModeLive:
				args.Choice = &choice
				fallback = FallbackNotLive + ": " + args.Mode
			default:
				args.Choice = &choice
			}
		}
	}
	if fallback != "" {
		d.FallbackReason = fallback
		args.FallbackReason = &fallback
	}
	res, err := s.Core.RecordAssessment(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("record_assessment: %w", err)
	}
	d.Assessment = res
	if !slices.Contains(route.Eligible, res.AppliedChoice) {
		return nil, fmt.Errorf("core applied_choice %q not in eligible %v", res.AppliedChoice, route.Eligible)
	}
	if res.UsedAdvisor && (s.Mode != ModeLive || fallback != "") {
		// Fail closed: in SHADOW (or after a fallback) the advisor must
		// never have steered the route.
		return nil, fmt.Errorf("core reports used_advisor=true in mode %s (fallback %q); refusing", s.Mode, fallback)
	}
	d.Applied = res.AppliedChoice
	d.Source = "core_fallback"
	if res.UsedAdvisor {
		d.Source = "advisor"
	}
	return d, nil
}

// safeAdvise runs the advisor in its own goroutine so that a panic or an
// advisor ignoring its context can never block or crash routing.
func safeAdvise(ctx context.Context, a DecisionAdvisor, q Question) (Advice, error) {
	type res struct {
		a   Advice
		err error
	}
	ch := make(chan res, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- res{err: fmt.Errorf("advisor panic: %v", r)}
			}
		}()
		a, e := a.Advise(ctx, q)
		ch <- res{a, e}
	}()
	select {
	case r := <-ch:
		return r.a, r.err
	case <-ctx.Done():
		return Advice{}, ctx.Err()
	}
}

// validate returns "" for well-formed advice.
func validate(a Advice) string {
	if a.Choice == "" {
		return "empty choice"
	}
	if a.ConfidenceBP < 0 || a.ConfidenceBP > 10000 {
		return fmt.Sprintf("confidence_bp %d out of 0..10000", a.ConfidenceBP)
	}
	var sum int64
	for opt, p := range a.ProbabilitiesBP {
		if p < 0 || p > 10000 {
			return fmt.Sprintf("probability for %s = %d out of 0..10000", opt, p)
		}
		if !slices.Contains(coreclient.RouteOptions, opt) {
			return fmt.Sprintf("probability for unknown option %q", opt)
		}
		sum += p
	}
	if len(a.ProbabilitiesBP) > 0 && sum != 10000 {
		return fmt.Sprintf("probabilities sum to %d bp, want 10000", sum)
	}
	return ""
}
