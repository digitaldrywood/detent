package releaseprovenance

import (
	"strings"
	"testing"
)

func TestVerifyGitHubEvidence(t *testing.T) {
	t.Parallel()

	manifest := Manifest{
		Schema:     Schema,
		Repository: "digitaldrywood/detent",
		Tag:        "v1.2.3",
		Commit:     testCommit,
		Checks:     []Check{{Name: "CI", Status: "completed", Conclusion: "success", CheckRunID: 42}},
	}
	valid := GitHubEvidence{
		RequiredChecks: []RepositoryRequirement{{Name: "CI", IntegrationID: 15368}},
		Checks:         []ObservedCheck{{Name: "CI", Status: "completed", Conclusion: "success", Commit: testCommit, CheckRunID: 42, IntegrationID: 15368}},
	}
	tests := []struct {
		name           string
		mutateManifest func(*Manifest)
		mutateEvidence func(*GitHubEvidence)
		wantErr        string
	}{
		{name: "exact authenticated evidence"},
		{name: "no repository requirements", mutateEvidence: func(e *GitHubEvidence) { e.RequiredChecks = nil }, wantErr: "no mandatory checks"},
		{name: "fabricated annotation omits required check", mutateEvidence: func(e *GitHubEvidence) {
			e.RequiredChecks = []RepositoryRequirement{{Name: "CI", IntegrationID: 15368}, {Name: "Security", IntegrationID: 15368}}
		}, wantErr: "omits authenticated repository requirement"},
		{name: "authenticated release-only check", mutateManifest: func(m *Manifest) {
			m.Checks = append(m.Checks, Check{Name: "Security", Status: "completed", Conclusion: "success", CheckRunID: 77})
		}, mutateEvidence: func(e *GitHubEvidence) {
			e.Checks = append(e.Checks, ObservedCheck{Name: "Security", Status: "completed", Conclusion: "success", Commit: testCommit, CheckRunID: 77, IntegrationID: 15368})
		}},
		{name: "missing immutable check run", mutateManifest: func(m *Manifest) { m.Checks[0].CheckRunID = 0 }, wantErr: "missing an immutable check-run ID"},
		{name: "wrong commit", mutateEvidence: func(e *GitHubEvidence) { e.Checks[0].Commit = testOtherCommit }, wantErr: "no authenticated"},
		{name: "stale check run", mutateEvidence: func(e *GitHubEvidence) { e.Checks[0].CheckRunID = 41 }, wantErr: "no authenticated"},
		{name: "wrong integration", mutateEvidence: func(e *GitHubEvidence) { e.Checks[0].IntegrationID = 99 }, wantErr: "no authenticated"},
		{name: "cancelled", mutateEvidence: func(e *GitHubEvidence) { e.Checks[0].Conclusion = "cancelled" }, wantErr: "no authenticated"},
		{name: "older success does not mask newer failed check run", mutateEvidence: func(e *GitHubEvidence) {
			e.Checks = append(e.Checks,
				ObservedCheck{Name: "CI", Status: "completed", Conclusion: "failure", Commit: testCommit, CheckRunID: 43, IntegrationID: 15368},
			)
		}, wantErr: "newer authenticated check run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			candidate := manifest
			candidate.Checks = append([]Check(nil), manifest.Checks...)
			evidence := valid
			evidence.RequiredChecks = append([]RepositoryRequirement(nil), valid.RequiredChecks...)
			evidence.Checks = append([]ObservedCheck(nil), valid.Checks...)
			if tt.mutateManifest != nil {
				tt.mutateManifest(&candidate)
			}
			if tt.mutateEvidence != nil {
				tt.mutateEvidence(&evidence)
			}
			err := VerifyGitHubEvidence(candidate, evidence)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("VerifyGitHubEvidence() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("VerifyGitHubEvidence() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
