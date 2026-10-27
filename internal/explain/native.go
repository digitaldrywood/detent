package explain

import (
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func FromNativeEvidence(e tracker.NativeRuntimeEvidence) IssueExplanation {
	issue := e.Issue
	r := IssueExplanation{ChangeSequence: e.ChangeSequence, Schema: SchemaVersion, Found: true, ObservedAt: e.ObservedAt,
		Identity:     Identity{ProjectID: string(issue.ProjectID), IssueID: string(issue.WorkItemID), Identifier: string(issue.ProjectID) + "#" + strconv.Itoa(issue.Number), Number: issue.Number, Title: issue.Title},
		CurrentLane:  Lane{Name: issue.State, ObservedAt: &issue.UpdatedAt, Freshness: SourceAvailable},
		Eligibility:  Eligibility{State: EligibilityUnknown, Source: SourceUnavailable, Refusals: []EligibilityDecision{}},
		Sessions:     Sessions{Source: SourceUnavailable},
		RequiredGate: Gate{State: GateUnavailable, SourceState: SourceUnavailable, Failures: []string{}, Running: []string{}},
		Sources:      []SourceStatus{{Name: "native_item", State: SourceAvailable}}, Evidence: []EvidenceReference{}, Reasons: []Reason{}}
	if event := e.NativeCandidateExclusion; event != nil {
		r.Sources = append(r.Sources, SourceStatus{Name: "native_candidate_exclusion", State: SourceAvailable})
		r.Evidence = append(r.Evidence, EvidenceReference{ID: event.ID, Kind: EvidenceScheduler, ObservedAt: &event.RecordedAt})
	}
	if event := e.LatestTransition; event != nil {
		r.LatestTransition = &Transition{EvidenceID: event.ID, From: event.Data.FromState, To: event.Data.ToState, At: event.RecordedAt, Source: "native", Reason: event.Data.Reason, ReasonDetail: event.Data.ReasonDetail, Actor: &Actor{Kind: event.Actor.Kind}, Provenance: Provenance{State: SourceAvailable, Schema: event.SchemaVersion, Origin: event.Actor.Kind, Initiator: event.Actor.Kind, Basis: "native_event", Trustworthy: true}}
		r.CurrentLane.EvidenceID = event.ID
		r.Evidence = append(r.Evidence, EvidenceReference{ID: event.ID, Kind: EvidenceWorkflow, ObservedAt: &event.RecordedAt})
	}
	if event := e.LatestDecision; event != nil && event.Data.Decision != nil {
		d := event.Data.Decision
		state := EligibilityUnknown
		if d.Outcome == "skipped" {
			state = EligibilityRefused
		}
		r.Eligibility.Latest = &EligibilityDecision{Historical: true, EvidenceID: event.ID, Source: d.Source, State: state, Outcome: d.Outcome, Reason: d.Reason, At: d.At}
		r.Eligibility.Source = SourceAvailable
		r.Evidence = append(r.Evidence, EvidenceReference{ID: event.ID, Kind: EvidenceScheduler, ObservedAt: &event.RecordedAt})
	}
	current := e.Scheduling
	state := EligibilityUnknown
	switch current.Outcome {
	case "skipped":
		state = EligibilityRefused
	case "ready":
		state = EligibilityEligible
	}
	r.Eligibility.Current = &EligibilityDecision{Source: current.Source, State: state, Outcome: current.Outcome, Reason: current.Reason, At: current.At}
	r.Eligibility.State = state
	if len(e.Admission) > 0 {
		r.Eligibility.Source = SourceAvailable
		r.Sources = append(r.Sources, SourceStatus{Name: "native_claim_candidate_snapshot", State: SourceAvailable})
	}
	for _, admission := range e.Admission {
		if admission.Outcome != "skipped" {
			continue
		}
		r.Eligibility.Refusals = append(r.Eligibility.Refusals, EligibilityDecision{
			AnsweredExclusion: admission.AnsweredExclusion, UnresolvedDependencies: admission.UnresolvedDependencies, Unavailable: admission.Unavailable,
			RunnerID: admission.RunnerID, Source: admission.Source, State: EligibilityRefused,
			Outcome: admission.Outcome, Reason: admission.Reason, ReasonCode: admission.ReasonCode, At: admission.ObservedAt,
		})
	}

	if a := e.Attempt; a != nil {
		r.Attempt = &Attempt{NativeID: a.AttemptID, EvidenceID: a.AttemptID, Selection: e.Selection, Status: a.Status, StartedAt: a.StartedAt, Freshness: SourceState(a.RuntimeFreshness)}
		if a.Status != "running" && a.Status != "interrupted" {
			r.Attempt.TerminalState = a.Status
			r.Attempt.CompletedAt = &a.UpdatedAt
		}
		if runtime := a.Runtime; runtime != nil {
			r.Attempt.ID = runtime.LocalAttemptID
			r.Attempt.Phase = runtime.Phase
			r.Attempt.HeartbeatAt = &runtime.HeartbeatAt
			if profile := runtime.Activity; profile != nil && profile.SessionID > 0 {
				r.Sessions.Source = SourceAvailable
				r.Sessions.Detent = &Session{EvidenceID: a.AttemptID, ID: strconv.FormatInt(profile.SessionID, 10), Selection: e.Selection, Backend: runtime.Identity.BackendKind}
				if profile.ProviderThreadRef != "" {
					r.Sessions.Provider = &Session{EvidenceID: a.AttemptID, ID: profile.ProviderThreadRef, Selection: e.Selection, Backend: runtime.Identity.BackendKind}
				}
			}
		}
		r.Sources = append(r.Sources, SourceStatus{Name: "native_attempt", State: SourceAvailable}, SourceStatus{Name: "runtime", State: SourceState(a.RuntimeFreshness)})
		r.Evidence = append(r.Evidence, EvidenceReference{ID: a.AttemptID, Kind: EvidenceAttempt, ObservedAt: &a.UpdatedAt})
		copyAttempt := *a
		copyAttempt.Runtime = a.Runtime.WithoutActivitySpans()
		e.Attempt = &copyAttempt
	}
	projectNativeGate(&r, e)
	for _, name := range e.Unavailable {
		r.Sources = append(r.Sources, SourceStatus{Name: name, State: SourceUnavailable})
	}
	e.Issue = e.Issue.RuntimeReference()
	r.NativeRuntime = &e
	return r
}

func projectNativeGate(r *IssueExplanation, e tracker.NativeRuntimeEvidence) {
	change := e.Change
	if change == nil {
		return
	}
	g := &r.RequiredGate
	g.SourceState, g.Source = SourceAvailable, "native_current_change_version"
	g.EvidenceID, g.ObservedAt = change.Change.CurrentVersion, &change.Change.UpdatedAt
	g.Reason, g.State = change.Summary.Status, GatePending
	if change.Summary.Status == "evidence_incomplete" {
		g.State, g.SourceState = GateUnknown, SourceUnavailable
	}
	if change.Summary.Status == "reviewed" || change.Summary.Status == "landed" {
		g.State = GateUnknown
		g.Reason = "External merge requirements have not been observed for the current version"
	}
	version := e.CurrentVersion
	var landing *tracker.NativeLandingReceipt
	if version != nil && version.ChangeID == change.Change.ID && version.VersionID == change.Change.CurrentVersion {
		g.HeadSHA = version.HeadSHA
		g.LocalValidation = nativeValidationObservation(version.Validation)
		landing = version.Landing
	}
	if landing == nil && e.Attempt != nil && e.Attempt.Runtime != nil {
		landing = e.Attempt.Runtime.Landing
	}
	g.ExternalMerge = &MergeObservation{State: GateUnknown, SourceState: SourceUnavailable, ChangeID: change.Change.ID, VersionID: change.Change.CurrentVersion, HeadSHA: g.HeadSHA}
	if landing != nil {
		merge := &MergeObservation{State: GateUnknown, SourceState: SourceLastKnown, ChangeID: landing.ChangeID, VersionID: landing.VersionID, HeadSHA: landing.HeadSHA, Reason: landing.RefusalKind, Detail: landing.Refusal}
		g.ExternalMerge = merge
		if !landing.ObservedAt.IsZero() {
			merge.ObservedAt = &landing.ObservedAt
		}
		merge.Historical = version == nil || landing.ChangeID != change.Change.ID || landing.VersionID != change.Change.CurrentVersion || landing.HeadSHA == "" || landing.HeadSHA != g.HeadSHA || landing.ObservedAt.Before(version.PublishedAt)
		if !merge.Historical && landing.Gate != nil {
			g.LocalValidation = nativeValidationObservation(landing.Gate)
		}
		if !merge.Historical && !landing.ObservedAt.IsZero() && !landing.ObservedAt.After(e.ObservedAt) && !landing.ObservedAt.Before(change.Change.UpdatedAt) {
			if event := e.LatestTransition; event != nil && landing.FromState != "" && landing.TargetState != "" && event.Data.FromState == landing.FromState && event.Data.ToState == landing.TargetState && !event.RecordedAt.Before(landing.ObservedAt) && !event.RecordedAt.After(e.ObservedAt) && strings.TrimSpace(event.Data.ReasonDetail) != "" {
				merge.Detail = event.Data.ReasonDetail
			}
			switch {
			case landing.Landed:
				merge.State, g.State = GatePassed, GatePassed
				g.Reason, g.Source, g.ObservedAt = "landed", "native_landing_receipt", merge.ObservedAt
			case landing.Waiting || landing.RefusalKind == "base_moved" || landing.Path == "batch_member" && landing.RefusalKind == "conflict" || landing.RefusalKind == "" && landing.CI != nil && landing.CI.State == "pending":
				merge.State, g.State = GatePending, GatePending
				g.SourceState, g.Reason, g.Source, g.ObservedAt = SourceLastKnown, landing.Refusal, "native_landing_receipt", merge.ObservedAt
			case landing.RefusalKind != "":
				merge.State, g.State = GateFailed, GateFailed
				g.SourceState, g.Source, g.ObservedAt = SourceLastKnown, "native_landing_receipt", merge.ObservedAt
				g.Reason = merge.Detail
				if strings.TrimSpace(g.Reason) == "" {
					g.Reason = merge.Reason
				}
				g.Failures = append(g.Failures, g.Reason)
				r.Reasons = append(r.Reasons, Reason{Code: merge.Reason, Detail: g.Reason, Action: "Address the recorded landing refusal before another landing attempt"})
			}
		}
	}
	if landed := change.Change.CurrentLanding(); landed != nil && (g.HeadSHA == "" || landed.HeadSHA == g.HeadSHA) {
		g.State, g.Reason, g.SourceState = GatePassed, "landed", SourceAvailable
		g.Source, g.ObservedAt = "native_current_change_landing", &landed.LandedAt
		g.ExternalMerge = &MergeObservation{State: GatePassed, SourceState: SourceAvailable, ChangeID: change.Change.ID, VersionID: landed.VersionID, HeadSHA: landed.HeadSHA, ObservedAt: &landed.LandedAt}
		g.Failures, r.Reasons = []string{}, []Reason{}
	}
	if change.Summary.Checks == "failed" || change.Summary.Checks == "failure" || change.Summary.NativeReview == "changes_requested" || g.LocalValidation != nil && g.LocalValidation.State == GateFailed {
		g.State = GateFailed
	}
}

func nativeValidationObservation(command *gate.CommandResult) *ValidationObservation {
	if command == nil {
		return nil
	}
	state := GateFailed
	if command.ExitCode == 0 {
		state = GatePassed
	}
	result := &ValidationObservation{State: state, ReceiptID: command.ReceiptID, Command: command.Command, HeadSHA: command.HeadSHA, TreeSHA: command.TreeSHA, ExitCode: command.ExitCode}
	if !command.FinishedAt.IsZero() {
		result.ObservedAt = &command.FinishedAt
	}
	return result
}
