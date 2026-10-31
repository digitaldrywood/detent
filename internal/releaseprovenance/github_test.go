package releaseprovenance

import (
	"crypto/sha256"
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
		{name: "unsigned override annotation cannot waive failed suite", mutateManifest: func(m *Manifest) {
			m.ValidationOverride = &ValidationOverride{Reason: "Forged owner reason", Status: "failure", StatusID: 9}
			m.Checks[0].Name = "release-security-audit"
		}, mutateEvidence: func(e *GitHubEvidence) {
			e.RequiredChecks = []RepositoryRequirement{{Name: "scheduled-full-ci"}, {Name: "release-security-audit"}}
			e.Checks[0].Name = "release-security-audit"
			e.Checks = append(e.Checks, ObservedCheck{Name: "scheduled-full-ci", Commit: testCommit, Status: "completed", Conclusion: "failure", StatusID: 9})
		}, wantErr: "verified override tag signature"},
		{name: "recorded override permits failed suite with authenticated audit", mutateManifest: func(m *Manifest) {
			m.ValidationOverride = &ValidationOverride{Reason: "Owner accepts failed validation", Status: "failure", StatusID: 9}
			m.Checks[0].Name = "release-security-audit"
		}, mutateEvidence: func(e *GitHubEvidence) {
			e.RequiredChecks = []RepositoryRequirement{{Name: "scheduled-full-ci"}, {Name: "release-security-audit"}}
			e.Checks[0].Name = "release-security-audit"
			e.Checks = append(e.Checks, ObservedCheck{Name: "scheduled-full-ci", Commit: testCommit, Status: "completed", Conclusion: "failure", StatusID: 9})
		}},
		{name: "recorded override cannot waive missing authenticated audit", mutateManifest: func(m *Manifest) {
			m.ValidationOverride = &ValidationOverride{Reason: "Owner accepts missing validation", Status: "missing"}
			m.Checks[0].Name = "release-security-audit"
		}, mutateEvidence: func(e *GitHubEvidence) {
			e.RequiredChecks = []RepositoryRequirement{{Name: "scheduled-full-ci"}, {Name: "release-security-audit"}}
		}, wantErr: "no authenticated"},
		{name: "recorded override cannot waive failed authenticated audit", mutateManifest: func(m *Manifest) {
			m.ValidationOverride = &ValidationOverride{Reason: "Owner accepts failed validation"}
			m.Checks[0].Name = "release-security-audit"
		}, mutateEvidence: func(e *GitHubEvidence) {
			e.RequiredChecks = []RepositoryRequirement{{Name: "scheduled-full-ci"}, {Name: "release-security-audit"}}
			e.Checks[0].Name = "release-security-audit"
			e.Checks[0].Conclusion = "failure"
		}, wantErr: "no authenticated"},
		{name: "no repository requirements", mutateEvidence: func(e *GitHubEvidence) { e.RequiredChecks = nil }, wantErr: "no mandatory checks"},
		{name: "fabricated annotation omits required check", mutateEvidence: func(e *GitHubEvidence) {
			e.RequiredChecks = []RepositoryRequirement{{Name: "CI", IntegrationID: 15368}, {Name: "Security", IntegrationID: 15368}}
		}, wantErr: "omits authenticated repository requirement"},
		{name: "authenticated release-only check", mutateManifest: func(m *Manifest) {
			m.Checks = append(m.Checks, Check{Name: "Security", Status: "completed", Conclusion: "success", CheckRunID: 77})
		}, mutateEvidence: func(e *GitHubEvidence) {
			e.Checks = append(e.Checks, ObservedCheck{Name: "Security", Status: "completed", Conclusion: "success", Commit: testCommit, CheckRunID: 77, IntegrationID: 15368})
		}},
		{name: "missing immutable evidence ID", mutateManifest: func(m *Manifest) { m.Checks[0].CheckRunID = 0 }, wantErr: "missing an immutable evidence ID"},
		{name: "wrong commit", mutateEvidence: func(e *GitHubEvidence) { e.Checks[0].Commit = testOtherCommit }, wantErr: "no authenticated"},
		{name: "stale check run", mutateEvidence: func(e *GitHubEvidence) { e.Checks[0].CheckRunID = 41 }, wantErr: "no authenticated"},
		{name: "wrong integration", mutateEvidence: func(e *GitHubEvidence) { e.Checks[0].IntegrationID = 99 }, wantErr: "no authenticated"},
		{name: "cancelled", mutateEvidence: func(e *GitHubEvidence) { e.Checks[0].Conclusion = "cancelled" }, wantErr: "no authenticated"},
		{name: "older success does not mask newer failed check run", mutateEvidence: func(e *GitHubEvidence) {
			e.Checks = append(e.Checks,
				ObservedCheck{Name: "CI", Status: "completed", Conclusion: "failure", Commit: testCommit, CheckRunID: 43, IntegrationID: 15368},
			)
		}, wantErr: "newer authenticated check run"},
		{name: "older success does not mask newer failed commit status", mutateManifest: func(m *Manifest) {
			m.Checks[0].CheckRunID = 0
			m.Checks[0].StatusID = 42
		}, mutateEvidence: func(e *GitHubEvidence) {
			e.RequiredChecks = []RepositoryRequirement{{Name: "CI"}}
			e.Checks = []ObservedCheck{
				{Name: "CI", Status: "completed", Conclusion: "success", Commit: testCommit, StatusID: 42},
				{Name: "CI", Status: "completed", Conclusion: "failure", Commit: testCommit, StatusID: 43},
			}
		}, wantErr: "newer authenticated commit status"},
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
			if candidate.ValidationOverride != nil && !strings.HasPrefix(tt.name, "unsigned override") {
				raw, err := Marshal(candidate)
				if err != nil {
					t.Fatal(err)
				}
				evidence.OverrideSignature = &OverrideSignature{manifestHash: sha256.Sum256(raw)}
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
