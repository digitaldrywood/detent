package progresscheck

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeBackend struct {
	decision Decision
	calls    int
}

func (b *fakeBackend) Decide(context.Context, Request) (Decision, error) {
	b.calls++
	return b.decision, nil
}

type fakeOwner struct {
	records    map[string]Record
	comments   int
	defects    map[string]string
	publishErr error
}

func (o *fakeOwner) Load(_ context.Context, _, _, id string) (Record, bool, error) {
	record, found := o.records[id]
	return record, found, nil
}

func (o *fakeOwner) Publish(_ context.Context, record Record) (Record, error) {
	if o.publishErr != nil {
		return Record{}, o.publishErr
	}
	if retained, found := o.records[record.RequestID]; found {
		return retained, nil
	}
	if o.records == nil {
		o.records = make(map[string]Record)
	}
	if o.defects == nil {
		o.defects = make(map[string]string)
	}
	if record.Decision.Outcome == SystemDefect {
		key := record.Decision.DefectKey
		if o.defects[key] == "" {
			o.defects[key] = "defect-issue"
		}
		record.DefectIssueID = o.defects[key]
	}
	record.CommentID = "progress-comment"
	o.comments++
	o.records[record.RequestID] = record
	return record, nil
}

func refusalRequest() Request {
	return Request{
		ProjectID: "example-project", IssueID: "example-issue", PolicyID: "policy", EvidenceRevision: "revision-1",
		SourceSHA: "head", BaseSHA: "base", Instance: "instance-1", AvailableSteps: []string{"implement", "security_audit", "rebase"},
		Attempts: []Attempt{
			{ID: "attempt-1", Step: "publication", Outcome: "failed", Error: "422: landing requires security audit evidence for its exact base and head", FailureClass: "publication", Attribution: "instance", Instance: "instance-1", SourceSHA: "head", BaseSHA: "base"},
			{ID: "attempt-2", Step: "publication", Outcome: "failed", Error: "422: landing requires security audit evidence for its exact base and head", FailureClass: "publication", Attribution: "instance", Instance: "instance-1", SourceSHA: "head", BaseSHA: "base"},
		},
	}
}

func TestCheckDecisionReplayAndEvidenceChange(t *testing.T) {
	tests := []struct {
		name          string
		decision      Decision
		changedSource bool
	}{
		{"changed source progresses", Decision{Outcome: Progressing, Reason: "Source changed since the failed attempt", Evidence: []string{"attempt-2"}}, true},
		{"1748 repeated publication refusal reroutes", Decision{Outcome: Reroute, Step: "security_audit", Reason: "Produce the missing audit receipt before repeating publication", Evidence: []string{"attempt-1", "attempt-2"}}, false},
		{"Detent refusal reports defect", Decision{Outcome: SystemDefect, Reason: "The publication protocol cannot carry the required audit evidence", Evidence: []string{"attempt-1", "attempt-2"}, DefectKey: "publication-audit-protocol", DefectTitle: "Publication cannot carry audit evidence", DefectBody: "Two unchanged-source attempts received the same 422 refusal"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := refusalRequest()
			if tt.changedSource {
				request.SourceSHA = "new-head"
			}
			backend := &fakeBackend{decision: tt.decision}
			owner := &fakeOwner{}
			first, checked, err := Check(t.Context(), request, backend, owner)
			if err != nil || !checked {
				t.Fatalf("Check = %v, %v", checked, err)
			}
			if first.Decision.Outcome != tt.decision.Outcome || first.Decision.Step != tt.decision.Step {
				t.Fatalf("decision = %+v", first.Decision)
			}
			replay, checked, err := Check(t.Context(), request, backend, owner)
			if err != nil || !checked || replay.RequestID != first.RequestID {
				t.Fatalf("replay = %+v, %v, %v", replay, checked, err)
			}
			if backend.calls != 1 || owner.comments != 1 {
				t.Fatalf("replay calls = %d, comments = %d", backend.calls, owner.comments)
			}
			if tt.decision.Outcome == SystemDefect {
				if len(owner.defects) != 1 || first.DefectIssueID == "" {
					t.Fatalf("defect publication = %+v", first)
				}
				request.EvidenceRevision = "revision-2"
				request.Comments = []Comment{{ID: "human-comment", Revision: "1", Human: true, Body: "The publication protocol has been repaired"}}
				backend.decision = Decision{Outcome: Progressing, Reason: "New human evidence requires another attempt", Evidence: []string{"human-comment"}}
				changed, checked, err := Check(t.Context(), request, backend, owner)
				if err != nil || !checked || changed.Decision.Outcome != Progressing || changed.RequestID == first.RequestID {
					t.Fatalf("changed evidence = %+v, %v", changed, err)
				}
				if backend.calls != 2 || len(owner.defects) != 1 {
					t.Fatalf("calls = %d, defects = %d", backend.calls, len(owner.defects))
				}
			}
		})
	}
}

func TestCheckSuccessAndPublicationFailure(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(map[bool]string{true: "success pays no turn", false: "publication failure does not return decision"}[success], func(t *testing.T) {
			request := refusalRequest()
			backend := &fakeBackend{decision: Decision{Outcome: Reroute, Step: "implement", Reason: "Repair source", Evidence: []string{"attempt-2"}}}
			owner := &fakeOwner{publishErr: errors.New("tracker unavailable")}
			if success {
				request.Attempts[1].Outcome = "success"
			}
			record, checked, err := Check(t.Context(), request, backend, owner)
			if success {
				if err != nil || checked || backend.calls != 0 {
					t.Fatalf("success = %v, %v, calls %d", checked, err, backend.calls)
				}
			} else if err == nil || checked || record.RequestID != "" || owner.comments != 0 {
				t.Fatalf("publication failure = %+v, %v, %v", record, checked, err)
			}
		})
	}
}

type textBackend string

func (b textBackend) Generate(context.Context, string, string) (string, error) { return string(b), nil }

func TestAIRejectsUnsafeOrMalformedDecisions(t *testing.T) {
	tests := []struct {
		name, output string
		valid        bool
	}{
		{"progressing step is ignored", `{"outcome":"progressing","step":"implement","reason":"New evidence","evidence":["attempt-2"]}`, true},
		{"typed reroute", `{"outcome":"reroute","step":"security_audit","reason":"Missing receipt","evidence":["attempt-2"]}`, true},
		{"unknown outcome", `{"outcome":"park","reason":"Repeated","evidence":["attempt-2"]}`, false},
		{"unavailable step", `{"outcome":"reroute","step":"bypass_audit","reason":"Missing receipt","evidence":["attempt-2"]}`, false},
		{"lease field", `{"outcome":"progressing","reason":"Go","evidence":["attempt-2"],"lease_owner":"new"}`, false},
		{"fabricated evidence", `{"outcome":"progressing","reason":"Go","evidence":["missing"]}`, false},
		{"missing evidence", `{"outcome":"progressing","reason":"Go"}`, false},
		{"missing defect body", `{"outcome":"system_defect","reason":"Protocol error","evidence":["attempt-2"],"defect_key":"protocol"}`, false},
		{"trailing object", `{"outcome":"progressing","reason":"Go","evidence":["attempt-2"]} {}`, false},
		{"oversized output", strings.Repeat("x", MaxOutputBytes+1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (AI{Backend: textBackend(tt.output)}).Decide(t.Context(), refusalRequest())
			if (err == nil) != tt.valid {
				t.Fatalf("error = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestCheckRejectsUnboundedEvidenceBeforeBackend(t *testing.T) {
	for _, mutate := range []struct {
		name  string
		apply func(*Request)
	}{
		{"attempt count", func(r *Request) { r.Attempts = make([]Attempt, MaxAttempts+1) }},
		{"receipt count", func(r *Request) { r.Receipts = make([]Receipt, MaxReceipts+1) }},
		{"comment count", func(r *Request) { r.Comments = make([]Comment, MaxComments+1) }},
		{"byte count", func(r *Request) { r.Attempts[0].Error = strings.Repeat("x", MaxInputBytes) }},
		{"receipt evidence collision", func(r *Request) { r.Receipts = []Receipt{{ID: "attempt-2"}} }},
		{"comment evidence collision", func(r *Request) { r.Comments = []Comment{{ID: "attempt-2"}} }},

		{"active attempt", func(r *Request) { r.Attempts[1].Outcome = "running" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			request := refusalRequest()
			mutate.apply(&request)
			backend := &fakeBackend{}
			_, checked, err := Check(t.Context(), request, backend, &fakeOwner{})
			if err == nil || checked || backend.calls != 0 {
				t.Fatalf("Check = %v, %v, calls %d", checked, err, backend.calls)
			}
		})
	}
}

func TestCheckSuccessfulOrCancelledWorkPaysNoTurnForStaleEvidence(t *testing.T) {
	for _, outcome := range []string{"success", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			request := refusalRequest()
			request.Attempts[len(request.Attempts)-1].Outcome = outcome
			request.Attempts[0].Error = strings.Repeat("x", MaxInputBytes+1)
			backend := &fakeBackend{}
			_, checked, err := Check(t.Context(), request, backend, &fakeOwner{})
			if err != nil || checked || backend.calls != 0 {
				t.Fatalf("Check = %v, %v, backend calls %d", checked, err, backend.calls)
			}
		})
	}
}
