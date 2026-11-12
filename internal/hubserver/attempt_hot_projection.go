package hubserver

import "github.com/digitaldrywood/detent/internal/tracker"

func applyHealthAttemptData(a *healthRateAttempt, data tracker.NativeRunData) {
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
	text, _ := failureFromAttempt(projected, a.Status)
	a.Recovery = healthRecoveryReason(text)
	if text != "" && a.Recovery == "" {
		a.Signature = normalizeFailureSignature(text)
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
