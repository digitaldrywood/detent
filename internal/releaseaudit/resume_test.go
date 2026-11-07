package releaseaudit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type resumeProvider struct {
	*fixtureProvider
	phases         []string
	preserveLedger bool
}

func (p *resumeProvider) Invoke(ctx context.Context, invocation Invocation, observe func(Usage) error) (Response, error) {
	p.phases = append(p.phases, invocation.Phase)
	response, err := p.fixtureProvider.Invoke(ctx, invocation, observe)
	if !p.preserveLedger || err != nil {
		return response, err
	}
	var input struct {
		Task   json.RawMessage `json:"task"`
		Ledger json.RawMessage `json:"ledger"`
	}
	if err := json.Unmarshal(invocation.Input, &input); err != nil {
		return response, err
	}
	switch invocation.Phase {
	case "coverage-critic", "final-coverage-critic":
		response.Output = input.Ledger
	case "hunt":
		var assigned map[string]json.RawMessage
		if err := json.Unmarshal(input.Task, &assigned); err != nil {
			return response, err
		}
		var completed []map[string]json.RawMessage
		if err := json.Unmarshal(fixtureLedger("covered", false), &completed); err != nil {
			return response, err
		}
		for _, key := range []string{"coverage_id", "canonical_refs", "subsystem"} {
			completed[0][key] = assigned[key]
		}
		response.Output, err = json.Marshal(struct {
			Ledger     []map[string]json.RawMessage `json:"ledger"`
			Candidates []Candidate                  `json:"candidates"`
		}{completed, []Candidate{}})
	}
	return response, err
}

func TestResumeRetainsCompletedHuntsAndUnresolvedCandidates(t *testing.T) {
	for _, verdict := range []string{"", "rejected", "needs_validation"} {
		t.Run("verdict="+verdict, func(t *testing.T) {
			source := pinnedFixture(t)
			root := t.TempDir()
			provider := &resumeProvider{fixtureProvider: &fixtureProvider{t: t, root: root, verdict: verdict, fail: "final-coverage-critic", usage: Usage{Input: 3, Output: 2, Cached: 1}}}
			if verdict != "" {
				provider.fail = "candidate-disprover"
			}
			records := &memoryRecords{}
			stage, err := New(provider, records, &recordedRepairs{records: records}, nil)
			if err != nil {
				t.Fatal(err)
			}
			request := Request{ResumeKey: strings.Repeat("a", 64), BaseCommit: strings.Repeat("b", 40), Identity: Identity{ProjectID: "prj_example", Repository: "example-org/example-project", Commit: source.Commit(), ReleaseSeries: "release/v1.0", RunID: "first", Attempt: "1", JobID: "audit"}, Source: source, Scratch: root, Allowance: Allowance{Invocations: 40, Usage: Usage{Input: 10000, Output: 10000, Cached: 10000}}}
			interrupted, err := stage.Run(t.Context(), request)
			if err == nil || interrupted.Status != "failed" {
				t.Fatalf("expected interrupted execution, got %s %v", interrupted.Status, err)
			}
			if verdict != "" && len(interrupted.Candidates) != 1 {
				t.Fatal("fixture did not retain its unresolved candidate")
			}
			before, err := json.Marshal(interrupted)
			if err != nil {
				t.Fatal(err)
			}
			provider.phases, provider.fail = nil, ""
			request.Resume = &interrupted
			result, err := stage.Run(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			want := "clean"
			if verdict == "needs_validation" {
				want = verdict
			}
			if result.Status != want || result.Sequence <= interrupted.Sequence || result.Invocations <= interrupted.Invocations || result.Usage.Input <= interrupted.Usage.Input {
				t.Fatalf("resume lost status/accounting: status=%s sequence=%d invocations=%d usage=%+v", result.Status, result.Sequence, result.Invocations, result.Usage)
			}
			after, err := json.Marshal(interrupted)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("resume mutated the caller-owned checkpoint")
			}
			counts := map[string]int{}
			for _, phase := range provider.phases {
				counts[phase]++
				if phase == "hunt" || strings.HasPrefix(phase, "recon-") {
					t.Fatalf("repeated completed work: %v", provider.phases)
				}
			}
			if counts["coverage-critic"] != 1 || counts["final-coverage-critic"] != 1 {
				t.Fatalf("resume skipped independent completeness checks: %v", counts)
			}
			if verdict != "" && (counts["candidate-disprover"] != 1 || counts["independent-record-verifier"] != 1 || len(result.Findings) != 1) {
				t.Fatalf("resume lost unresolved candidate verification: phases=%v findings=%s", counts, result.Findings)
			}
		})
	}
}

func TestResumeRejectsMismatchedExecutionBeforeCallingProvider(t *testing.T) {
	source := pinnedFixture(t)
	for _, change := range []struct {
		name  string
		apply func(*Request, *Snapshot)
	}{
		{"missing key", func(r *Request, _ *Snapshot) { r.ResumeKey = "" }},
		{"tooling model scope key", func(r *Request, _ *Snapshot) { r.ResumeKey = strings.Repeat("b", 64) }},
		{"base", func(r *Request, _ *Snapshot) { r.BaseCommit = strings.Repeat("c", 40) }},
		{"head", func(_ *Request, s *Snapshot) { s.Identity.Commit = strings.Repeat("c", 40) }},
		{"repository", func(_ *Request, s *Snapshot) { s.Identity.Repository = "other/project" }},
		{"project", func(_ *Request, s *Snapshot) { s.Identity.ProjectID = "other" }},
		{"series", func(_ *Request, s *Snapshot) { s.Identity.ReleaseSeries = "release/v2.0" }},
		{"attempt", func(_ *Request, s *Snapshot) { s.Identity.Attempt = "2" }},
		{"bundle commit", func(_ *Request, s *Snapshot) { s.BundleCommit = strings.Repeat("c", 40) }},
		{"bundle digest", func(_ *Request, s *Snapshot) { s.BundleDigest = strings.Repeat("c", 64) }},
		{"completed", func(_ *Request, s *Snapshot) { s.Status = "clean" }},
		{"accounting", func(_ *Request, s *Snapshot) { s.Invocations = 100 }},
		{"ledger absent", func(_ *Request, s *Snapshot) { s.Ledger = json.RawMessage(`[]`) }},
		{"ledger invalid", func(_ *Request, s *Snapshot) { s.Ledger = json.RawMessage(`[{"coverage_id":"invalid"}]`) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			root := t.TempDir()
			provider := &resumeProvider{fixtureProvider: &fixtureProvider{t: t, root: root}}
			records := &memoryRecords{}
			stage, err := New(provider, records, &recordedRepairs{records: records}, nil)
			if err != nil {
				t.Fatal(err)
			}
			identity := Identity{ProjectID: "prj_example", Repository: "example-org/example-project", Commit: source.Commit(), ReleaseSeries: "release/v1.0", RunID: "first", Attempt: "1", JobID: "audit"}
			prior := Snapshot{ResumeKey: strings.Repeat("a", 64), BaseCommit: strings.Repeat("b", 40), Identity: identity, BundleCommit: BundleCommit, BundleDigest: BundleDigest, Sequence: 1, Status: "failed", Ledger: fixtureLedger("covered", false)}
			request := Request{ResumeKey: prior.ResumeKey, Resume: &prior, BaseCommit: prior.BaseCommit, Identity: identity, Source: source, Scratch: root, Allowance: Allowance{Invocations: 30, Usage: Usage{Input: 10000, Output: 10000, Cached: 10000}}}
			change.apply(&request, &prior)
			_, err = stage.Run(t.Context(), request)
			if err == nil || errors.Is(err, ErrAllowance) || len(provider.phases) != 0 || records.publications != 0 {
				t.Fatalf("mismatch not refused before work: err=%v phases=%v publications=%d", err, provider.phases, records.publications)
			}
		})
	}
}

func TestResumeRunsOnlyUnfinishedHunts(t *testing.T) {
	source := pinnedFixture(t)
	root := t.TempDir()
	provider := &resumeProvider{fixtureProvider: &fixtureProvider{t: t, root: root, usage: Usage{Input: 3, Output: 2, Cached: 1}}, preserveLedger: true}
	records := &memoryRecords{}
	stage, err := New(provider, records, &recordedRepairs{records: records}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var completed, planned []json.RawMessage
	if err := json.Unmarshal(fixtureLedger("covered", false), &completed); err != nil {
		t.Fatal(err)
	}
	other := strings.ReplaceAll(string(fixtureLedger("planned", false)), "route::owner::api::access-control", "route::owner::secondary::access-control")
	other = strings.ReplaceAll(other, `"subsystem":"api"`, `"subsystem":"secondary"`)
	other = strings.ReplaceAll(other, `"subsystem":"API"`, `"subsystem":"Secondary API"`)
	if err := json.Unmarshal([]byte(other), &planned); err != nil {
		t.Fatal(err)
	}
	ledger, err := json.Marshal(append(completed, planned...))
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{ProjectID: "prj_example", Repository: "example-org/example-project", Commit: source.Commit(), ReleaseSeries: "release/v1.0", RunID: "first", Attempt: "1", JobID: "audit"}
	prior := Snapshot{ResumeKey: strings.Repeat("a", 64), Identity: identity, BundleCommit: BundleCommit, BundleDigest: BundleDigest, Sequence: 2, Status: "failed", Ledger: ledger, Invocations: 6, Usage: Usage{Input: 18, Output: 12, Cached: 6}}
	request := Request{ResumeKey: prior.ResumeKey, Resume: &prior, Identity: identity, Source: source, Scratch: root, Allowance: Allowance{Invocations: 30, Usage: Usage{Input: 10000, Output: 10000, Cached: 10000}}}
	result, err := stage.Run(t.Context(), request)
	if err != nil || result.Status != "clean" {
		var invalid *documentValidationError
		if errors.As(err, &invalid) {
			t.Log(invalid.diagnostics)
		}
		t.Fatalf("unfinished work did not complete: status=%s err=%v", result.Status, err)
	}
	hunts := 0
	for _, phase := range provider.phases {
		if phase == "hunt" {
			hunts++
		}
	}
	if hunts != 1 {
		t.Fatalf("completed work repeated or unfinished work skipped: phases=%v", provider.phases)
	}
}
