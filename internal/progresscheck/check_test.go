package progresscheck

import "testing"

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
