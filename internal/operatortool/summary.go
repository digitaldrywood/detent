package operatortool

import (
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type NativeItemSummary struct {
	Identifier             string           `json:"identifier"`
	Number                 int              `json:"number"`
	State                  string           `json:"state"`
	Priority               *int             `json:"priority"`
	Title                  string           `json:"title"`
	Type                   string           `json:"type"`
	Labels                 []string         `json:"labels"`
	LastActivityAt         time.Time        `json:"last_activity_at"`
	Revision               tracker.Revision `json:"revision,string"`
	LatestTransitionReason string           `json:"latest_transition_reason"`
}

type NativeSummaryPage struct {
	tracker.Page[NativeItemSummary]
	Total int `json:"total"`
}

func NativeSummaryPageView(page tracker.NativeIssuePage) NativeSummaryPage {
	out := NativeSummaryPage{Page: tracker.Page[NativeItemSummary]{Items: []NativeItemSummary{}, NextCursor: page.NextCursor}, Total: page.Total}
	for _, issue := range page.Items {
		out.Items = append(out.Items, NativeItemSummary{
			Identifier: string(issue.ProjectID) + "#" + strconv.Itoa(issue.Number),
			Number: issue.Number, State: issue.State, Priority: issue.Priority,
			Title: issue.Title, Type: issue.Type, Labels: issue.Labels,
			LastActivityAt: issue.LastActivityAt, Revision: issue.Revision,
			LatestTransitionReason: issue.LatestTransitionReason,
		})
	}
	return out
}

type explanationSummary struct {
	Schema           int                       `json:"schema"`
	Found            bool                      `json:"found"`
	ObservedAt       time.Time                 `json:"observed_at"`
	Identity         explain.Identity          `json:"identity"`
	CurrentLane      explain.Lane              `json:"current_lane"`
	LatestTransition *transitionSummary        `json:"latest_transition,omitempty"`
	Eligibility      eligibilitySummary        `json:"eligibility"`
	Attempt          *attemptSummary           `json:"attempt,omitempty"`
	FailureSignature *explain.FailureSignature `json:"failure_signature,omitempty"`
	RequiredGate     gateSummary               `json:"required_gate"`
}

type transitionSummary struct {
	From         string    `json:"from,omitempty"`
	To           string    `json:"to"`
	At           time.Time `json:"at"`
	Reason       string    `json:"reason"`
	ReasonDetail string    `json:"reason_detail"`
}

type eligibilitySummary struct {
	State   explain.EligibilityState `json:"state"`
	Source  explain.SourceState      `json:"source_state"`
	Current *decisionSummary         `json:"current,omitempty"`
	Latest  *decisionSummary         `json:"latest,omitempty"`
}

type decisionSummary struct {
	Outcome    string    `json:"outcome"`
	Reason     string    `json:"reason"`
	At         time.Time `json:"at"`
	Historical bool      `json:"historical,omitempty"`
}

type attemptSummary struct {
	NativeID  string              `json:"native_id,omitempty"`
	ID        int64               `json:"id,omitempty"`
	RunnerID  string              `json:"runner_id,omitempty"`
	Role      string              `json:"role,omitempty"`
	Selection string              `json:"selection"`
	Freshness explain.SourceState `json:"freshness,omitempty"`
	StartedAt time.Time           `json:"started_at"`
	Status    string              `json:"status"`
}

type gateSummary struct {
	State       explain.GateState   `json:"state"`
	SourceState explain.SourceState `json:"source_state"`
	Reason      string              `json:"reason,omitempty"`
}

func compactExplanation(in explain.IssueExplanation) explanationSummary {
	out := explanationSummary{
		Schema: in.Schema, Found: in.Found, ObservedAt: in.ObservedAt,
		Identity: in.Identity, CurrentLane: in.CurrentLane, FailureSignature: in.FailureSignature,
		Eligibility: eligibilitySummary{State: in.Eligibility.State, Source: in.Eligibility.Source},
		RequiredGate: gateSummary{State: in.RequiredGate.State, SourceState: in.RequiredGate.SourceState, Reason: in.RequiredGate.Reason},
	}
	if t := in.LatestTransition; t != nil {
		out.LatestTransition = &transitionSummary{From: t.From, To: t.To, At: t.At, Reason: t.Reason, ReasonDetail: t.ReasonDetail}
	}
	for _, pair := range []struct {
		in  *explain.EligibilityDecision
		out **decisionSummary
	}{{in.Eligibility.Current, &out.Eligibility.Current}, {in.Eligibility.Latest, &out.Eligibility.Latest}} {
		if d := pair.in; d != nil {
			*pair.out = &decisionSummary{Outcome: d.Outcome, Reason: d.Reason, At: d.At, Historical: d.Historical}
		}
	}
	if a := in.Attempt; a != nil {
		out.Attempt = &attemptSummary{NativeID: a.NativeID, ID: a.ID, Selection: a.Selection, Freshness: a.Freshness, StartedAt: a.StartedAt, Status: a.Status}
		if in.NativeRuntime != nil && in.NativeRuntime.Attempt != nil {
			native := in.NativeRuntime.Attempt
			out.Attempt.RunnerID = native.RunnerID
			if native.Identity != nil {
				out.Attempt.Role = native.Identity.Role
			}
		}
	}
	return out
}
