package progresscheck

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
)

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

func TestEmptyLandingCompletionRequiresAcceptanceEvidence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*Request, *Decision)
		valid  bool
	}{
		{name: "passing regression on the base", valid: true},
		{name: "recorded incorporated reviewed delta", valid: true, mutate: func(r *Request, d *Decision) { d.Evidence = []string{"base-proof"} }},
		{name: "nonempty delta", mutate: func(r *Request, d *Decision) { r.EmptyDelta = false }},
		{name: "missing landing refusal", mutate: func(r *Request, d *Decision) { r.Attempts[1].LandingRefusal = "" }},
		{name: "real undelivered work", mutate: func(r *Request, d *Decision) { r.Attempts[1].LandingRefusal = "conflict" }},
		{name: "review or audit gate pending", mutate: func(r *Request, d *Decision) { r.AvailableSteps = []string{"implement", "merge"} }},
		{name: "empty delta alone", mutate: func(r *Request, d *Decision) { d.Evidence = []string{"empty-version"} }},
		{name: "refusal alone", mutate: func(r *Request, d *Decision) { d.Evidence = []string{"attempt-2:landing"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := refusalRequest()
			request.EmptyDelta = true
			request.AvailableSteps = append(request.AvailableSteps, CompleteStep)
			request.Attempts[1].Outcome = "success"
			request.Attempts[1].LandingRefusal = "nothing_to_land"
			request.Receipts = []Receipt{{ID: "attempt-2:landing", Step: "landing", Outcome: "nothing_to_land"}, {ID: "empty-version", Step: "source"}, {ID: "base-test", Step: "check", Outcome: "success", HeadSHA: "base", Detail: "Acceptance regression passes"}}
			request.Comments = []Comment{{ID: "base-proof", Body: "Previously reviewed delta is incorporated in base"}}
			decision := Decision{Outcome: Reroute, Step: CompleteStep, Diagnosis: "base-test and base-proof show the required behavior is already on the base", Instruction: "Complete the issue using the verified base acceptance evidence", Reason: "Acceptance regression passes on the base", Evidence: []string{"base-test"}}
			if test.mutate != nil {
				test.mutate(&request, &decision)
			}
			if err := validateDecision(request, decision); (err == nil) != test.valid {
				t.Fatalf("completion validation error=%v want valid=%v", err, test.valid)
			}
		})
	}
}

func TestRetainedLegacyDecisionsRemainValid(t *testing.T) {
	for _, outcome := range []Outcome{Progressing, Reroute, SystemDefect} {
		t.Run(string(outcome), func(t *testing.T) {
			request := refusalRequest()
			decision := Decision{Outcome: outcome, Reason: "Retained judgment", Evidence: []string{"attempt-2"}}
			if outcome == Reroute {
				decision.Step = "implement"
			}
			if outcome == SystemDefect {
				decision.DefectKey, decision.DefectTitle, decision.DefectBody = "key", "title", "body"
			}
			id, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			record := Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: decision, CommentID: "comment", DefectIssueID: "defect"}
			if err := validateDecision(request, decision); err == nil {
				t.Fatal("new AI decision accepted legacy shape")
			}
			if err := record.Validate(request); err != nil {
				t.Fatalf("retained legacy record invalid: %v", err)
			}
			if err := record.ValidatePublication(request); err == nil {
				t.Fatal("new publication accepted legacy shape")
			}
		})
	}
}

func TestRecordValidatePublicationRequiresDecisionEvidence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		outcome Outcome
		mutate  func(*Decision)
	}{
		{"cited reroute with instruction", Reroute, nil},
		{"uncited diagnosis", Reroute, func(d *Decision) { d.Diagnosis = "The validation receipt is missing" }},
		{"missing reroute instruction", Reroute, func(d *Decision) { d.Instruction = "" }},
		{"progressing with convergence evidence", Progressing, nil},
		{"missing convergence evidence", Progressing, func(d *Decision) { d.Convergence = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := refusalRequest()
			id, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			decision := Decision{
				Outcome:   test.outcome,
				Reason:    "Resolve the missing validation receipt",
				Diagnosis: "attempt-2 failed because the validation receipt was missing",
				Evidence:  []string{"attempt-2"},
			}
			if test.outcome == Reroute {
				decision.Step = "implement"
				decision.Instruction = "Run the missing validation and retain its receipt"
			} else {
				decision.Convergence = "The repaired source passed validation and now has its receipt"
			}
			if test.mutate != nil {
				test.mutate(&decision)
			}
			record := Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: decision, CommentID: "comment"}
			if err := record.ValidatePublication(request); (err != nil) != (test.mutate != nil) {
				t.Fatalf("ValidatePublication error = %v, want rejection = %t", err, test.mutate != nil)
			}
		})
	}
}

func TestRequestIDRejectsInvalidEvidence(t *testing.T) {
	t.Parallel()
	if id, err := refusalRequest().ID(); err != nil || id == "" {
		t.Fatalf("valid request ID = %q, error = %v", id, err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Request)
	}{
		{"attempt count", func(r *Request) {
			attempts := make([]Attempt, MaxAttempts+1)
			for i := range attempts {
				attempts[i] = Attempt{ID: strconv.Itoa(i), Outcome: "failed"}
			}
			r.Attempts = attempts
		}},
		{"receipt count", func(r *Request) {
			r.Receipts = make([]Receipt, MaxReceipts+1)
			for i := range r.Receipts {
				r.Receipts[i].ID = strconv.Itoa(i)
			}
		}},
		{"comment count", func(r *Request) {
			r.Comments = make([]Comment, MaxComments+1)
			for i := range r.Comments {
				r.Comments[i].ID = strconv.Itoa(i)
			}
		}},
		{"byte count", func(r *Request) { r.Attempts[0].Error = strings.Repeat("x", MaxInputBytes) }},
		{"receipt ID collides with attempt ID", func(r *Request) { r.Receipts = []Receipt{{ID: "attempt-2"}} }},
		{"comment ID collides with attempt ID", func(r *Request) { r.Comments = []Comment{{ID: "attempt-2"}} }},
		{"nonterminal attempt", func(r *Request) { r.Attempts[1].Outcome = "running" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := refusalRequest()
			test.mutate(&request)
			if id, err := request.ID(); err == nil || id != "" {
				t.Fatalf("invalid request ID = %q, error = %v", id, err)
			}
		})
	}
}

func TestRoundPublicationReassessesDefiniteRejection(t *testing.T) {
	var assessed int
	var published []string
	_, err := RunRound(t.Context(), func(error) error { return nil },
		func(context.Context) error { return nil },
		func(_ context.Context, input RoundInput) error {
			published = append(published, input.RoundID)
			switch len(published) {
			case 1:
				return errors.New("reply unavailable")
			case 2:
				return ErrRoundVerdictRejected
			default:
				return nil
			}
		},
		func(ctx context.Context) (RoundAssessment, RoundInput, error) {
			assessed++
			return AssessRound(ctx, RoundEvidence{Phase: "implement", Criteria: []gate.CriterionEvidence{
				{Criterion: "Small criterion"},
				{Criterion: strings.Repeat("large", MaxOutputBytes)},
			}}, RoundAssessment{Action: RoundContinue, NextPhase: "implement"}, false, false,
				func(context.Context, string) (string, error) {
					return `{"action":"continue","next_phase":"implement","reason":"continue"}`, nil
				})
		})
	if err != nil || assessed != 2 || len(published) != 3 || published[0] != published[1] || published[1] == published[2] {
		t.Fatalf("assessment calls=%d publication IDs=%v error=%v", assessed, published, err)
	}
}

func TestRoundCriterionEvidenceFitsPublicationBound(t *testing.T) {
	_, input, err := AssessRound(t.Context(), RoundEvidence{Phase: "implement", Criteria: []gate.CriterionEvidence{
		{Criterion: "Small criterion"},
		{Criterion: strings.Repeat("large", MaxOutputBytes)},
	}}, RoundAssessment{Action: RoundContinue, NextPhase: "implement"}, false, false,
		func(context.Context, string) (string, error) {
			return `{"action":"continue","next_phase":"implement","reason":"continue"}`, nil
		})
	data, marshalErr := json.Marshal(input)
	if err != nil || marshalErr != nil || len(data) > MaxOutputBytes || len(input.Criteria) != 1 || input.Criteria[0].Criterion != "Small criterion" {
		t.Fatalf("serialized bytes=%d criteria=%d errors=%v / %v", len(data), len(input.Criteria), err, marshalErr)
	}
}
