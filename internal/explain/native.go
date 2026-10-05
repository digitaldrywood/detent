package explain

import (
	"strconv"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func FromNativeEvidence(e tracker.NativeRuntimeEvidence) IssueExplanation {
	issue := e.Issue
	r := IssueExplanation{Schema: SchemaVersion, Found: true, ObservedAt: e.ObservedAt,
		Identity:     Identity{ProjectID: string(issue.ProjectID), IssueID: string(issue.WorkItemID), Identifier: string(issue.ProjectID) + "#" + strconv.Itoa(issue.Number), Number: issue.Number, Title: issue.Title},
		CurrentLane:  Lane{Name: issue.State, ObservedAt: &issue.UpdatedAt, Freshness: SourceAvailable},
		Eligibility:  Eligibility{State: EligibilityUnknown, Source: SourceUnavailable, Refusals: []EligibilityDecision{}},
		Sessions:     Sessions{Source: SourceUnavailable},
		RequiredGate: Gate{State: GateUnavailable, SourceState: SourceUnavailable, Failures: []string{}, Running: []string{}},
		Sources:      []SourceStatus{{Name: "native_item", State: SourceAvailable}}, Evidence: []EvidenceReference{}, Reasons: []Reason{}}
	if event := e.LatestTransition; event != nil {
		r.LatestTransition = &Transition{EvidenceID: event.ID, From: event.Data.FromState, To: event.Data.ToState, At: event.RecordedAt, Source: "native", Reason: event.Data.Reason, Actor: &Actor{Kind: event.Actor.Kind}, Provenance: Provenance{State: SourceAvailable, Schema: event.SchemaVersion, Origin: event.Actor.Kind, Initiator: event.Actor.Kind, Basis: "native_event", Trustworthy: true}}
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
			UnresolvedDependencies: admission.UnresolvedDependencies, Unavailable: admission.Unavailable,
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
	if change := e.Change; change != nil {
		r.RequiredGate.SourceState = SourceAvailable
		r.RequiredGate.Source = "native_current_change_version"
		r.RequiredGate.EvidenceID = change.Change.CurrentVersion
		r.RequiredGate.ObservedAt = &change.Change.UpdatedAt
		r.RequiredGate.Reason = change.Summary.Status
		r.RequiredGate.State = GatePending
		if change.Summary.Status == "reviewed" || change.Summary.Status == "landed" {
			r.RequiredGate.State = GatePassed
		}
		if change.Summary.Checks == "failed" || change.Summary.NativeReview == "changes_requested" {
			r.RequiredGate.State = GateFailed
		}
	}
	for _, name := range e.Unavailable {
		r.Sources = append(r.Sources, SourceStatus{Name: name, State: SourceUnavailable})
	}
	e.Issue = e.Issue.RuntimeReference()
	r.NativeRuntime = &e
	return r
}
