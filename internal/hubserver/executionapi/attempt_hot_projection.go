package executionapi

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reports"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func AttemptFailureSummary(data tracker.NativeRunData, status string) *leasestore.AttemptFailure {
	example, class := reports.FailureFromAttempt(data, status)
	summary := &leasestore.AttemptFailure{}
	if example = hubapi.BoundedFailureText(strings.ReplaceAll(example, "\x00", "\uFFFD"), leasestore.MaxAttemptFailureExampleBytes); example == "" {
		return summary
	}
	summary.Example, summary.Class = example, class
	if data.Runtime != nil && data.Runtime.Landing != nil && !data.Runtime.Landing.ObservedAt.IsZero() {
		summary.LandingObservedAt = new(data.Runtime.Landing.ObservedAt.UTC())
	}
	return summary
}

func ApplyHealthAttemptData(a *healthsignals.RateAttempt, data tracker.NativeRunData) {
	a.Session = data.SessionID
	if data.TerminalFailure != nil {
		a.Stage = data.TerminalFailure.Stage
	}
	if f := data.Finalization; f != nil && f.Settled && f.Changed {
		a.SourceVersion = f.VersionID
	}
	if f := data.TerminalFailure; a.Status == "failed" && f != nil && f.Stage == "pre_provider" && f.Attribution == "instance" {
		a.InstanceSetupFailure = true
	}
	projected := tracker.NativeRunData{TerminalFailure: data.TerminalFailure, Finalization: data.Finalization}
	if data.Runtime != nil {
		projected.Runtime = &tracker.NativeRuntimeObservation{HeartbeatAt: data.Runtime.HeartbeatAt, Landing: data.Runtime.Landing}
	}
	text, _ := reports.FailureFromAttempt(projected, a.Status)
	a.Recovery = healthsignals.New(healthsignals.Deps{NormalizeFailureSignature: reports.NormalizeFailureSignature}).RecoveryReason(text)
	if text != "" && a.Recovery == "" {
		a.Signature = reports.NormalizeFailureSignature(text)
	}
	if r := data.Runtime; r != nil {
		a.Heartbeat = r.HeartbeatAt
		if l := r.Landing; l != nil {
			if l.Landed || l.RefusalKind != "" {
				a.LandingAt = l.ObservedAt
				a.Landing, a.Landed, a.Conflict = true, l.Landed, !l.Landed && l.RefusalKind == "conflict"
				if l.Landed && l.ChangeID != "" {
					a.LandingKey = l.ChangeID + "\x00" + l.VersionID + "\x00" + l.MergeSHA
				}
			}
		}
	}
}
