package progresscheck

import (
	"strconv"
	"strings"
	"testing"
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
