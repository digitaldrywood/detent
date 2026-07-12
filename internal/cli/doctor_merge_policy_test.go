package cli

import (
	"context"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestDoctorRepositoryMergePolicyWarning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		mergeMethodYAML string
		settings        ghconnector.RepositoryMergeSettings
		wantDetail      string
		wantFix         string
		wantPatch       bool
	}{
		{
			name:            "declared method forbidden",
			mergeMethodYAML: "squash",
			settings:        ghconnector.RepositoryMergeSettings{AllowMergeCommit: true},
			wantDetail:      "digitaldrywood/detent declared merge_method=squash is forbidden by repo settings merge_commit=true,squash=false,rebase=false",
			wantFix:         "gh api --method PATCH repos/digitaldrywood/detent -F allow_merge_commit=false -F allow_squash_merge=true -F allow_rebase_merge=false",
		},
		{
			name:            "declared method loose",
			mergeMethodYAML: "squash",
			settings:        ghconnector.RepositoryMergeSettings{AllowMergeCommit: true, AllowSquashMerge: true},
			wantDetail:      "digitaldrywood/detent declared merge_method=squash is loose because repo settings merge_commit=true,squash=true,rebase=false permit additional methods",
			wantFix:         "gh api --method PATCH repos/digitaldrywood/detent -F allow_merge_commit=false -F allow_squash_merge=true -F allow_rebase_merge=false",
		},
		{
			name:       "undeclared method ambiguous",
			settings:   ghconnector.RepositoryMergeSettings{AllowMergeCommit: true, AllowSquashMerge: true, AllowRebaseMerge: true},
			wantDetail: "digitaldrywood/detent ambiguous merge policy: repo settings merge_commit=true,squash=true,rebase=true allow multiple methods and WORKFLOW.md declares none; agent-side auto-detection can produce mixed history",
			wantFix:    "add `merge_method: squash` under `deliverable:` in WORKFLOW.md",
			wantPatch:  true,
		},
		{
			name:       "undeclared ambiguity chooses enabled method",
			settings:   ghconnector.RepositoryMergeSettings{AllowMergeCommit: true, AllowRebaseMerge: true},
			wantDetail: "digitaldrywood/detent ambiguous merge policy: repo settings merge_commit=true,squash=false,rebase=true allow multiple methods and WORKFLOW.md declares none; agent-side auto-detection can produce mixed history",
			wantFix:    "add `merge_method: merge` under `deliverable:` in WORKFLOW.md",
			wantPatch:  true,
		},
		{
			name:       "undeclared effective default forbidden",
			settings:   ghconnector.RepositoryMergeSettings{AllowMergeCommit: true},
			wantDetail: "digitaldrywood/detent effective default merge_method=squash is forbidden by repo settings merge_commit=true,squash=false,rebase=false and WORKFLOW.md declares none",
			wantFix:    "gh api --method PATCH repos/digitaldrywood/detent -F allow_merge_commit=false -F allow_squash_merge=true -F allow_rebase_merge=false",
		},
		{
			name:            "exact declared method",
			mergeMethodYAML: "squash",
			settings:        ghconnector.RepositoryMergeSettings{AllowSquashMerge: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deliverable := parseDoctorMergePolicyDeliverable(t, tt.mergeMethodYAML)
			gotDetail, gotFix, gotPatch := doctorRepositoryMergePolicyWarning("digitaldrywood/detent", deliverable, tt.settings)
			if gotDetail != tt.wantDetail {
				t.Fatalf("detail = %q, want %q", gotDetail, tt.wantDetail)
			}
			if gotFix != tt.wantFix {
				t.Fatalf("fix = %q, want %q", gotFix, tt.wantFix)
			}
			if (gotPatch != nil) != tt.wantPatch {
				t.Fatalf("patch present = %t, want %t", gotPatch != nil, tt.wantPatch)
			}
			if gotPatch != nil && gotPatch.Path != "deliverable.merge_method" {
				t.Fatalf("patch = %#v, want deliverable.merge_method", gotPatch)
			}
		})
	}
}

func TestCheckDoctorProjectSkipsRepositoryMergePolicyForArtifact(t *testing.T) {
	t.Parallel()

	cfg := validDoctorWorkflow("/repo")
	cfg.Tracker.Kind = workflowconfig.TrackerGitHub
	cfg.Deliverable.Kind = workflowconfig.DeliverableArtifact
	settingsReads := 0
	checks := checkDoctorProject(context.Background(), globalconfig.Project{ID: "artifact", Workflow: "WORKFLOW.md", Workdir: "/repo"}, doctorDeps{
		loadWorkflow: func(string) (workflowconfig.Workflow, error) {
			return workflowconfig.Workflow{Config: cfg}, nil
		},
		gitWorkTree: func(context.Context, string) error { return nil },
		githubReadiness: func(context.Context, ghconnector.Config, ghconnector.ReadinessConfig) ([]ghconnector.ReadinessCheck, error) {
			return nil, nil
		},
		githubMergeSettings: func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryMergeSettings, error) {
			settingsReads++
			return ghconnector.RepositoryMergeSettings{}, nil
		},
	}, RuntimeSecret{}, false)

	if settingsReads != 0 {
		t.Fatalf("settings reads = %d, want 0", settingsReads)
	}
	for _, check := range checks {
		if strings.Contains(check.Name, "repository merge policy") {
			t.Fatalf("artifact checks include merge policy: %#v", check)
		}
	}
}

func TestCheckDoctorRepositoryMergePolicyFeedsProposalPipeline(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerGitHub
	cfg.Tracker.Repository = "digitaldrywood/detent"
	check := checkDoctorRepositoryMergePolicy(context.Background(), "detent", globalconfig.Project{}, cfg, doctorDeps{
		githubMergeSettings: func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryMergeSettings, error) {
			return ghconnector.RepositoryMergeSettings{AllowMergeCommit: true, AllowSquashMerge: true}, nil
		},
	})

	if check.Status != doctorWarn {
		t.Fatalf("Status = %s, want WARN", check.Status)
	}
	if len(check.WorkflowOptimization.Findings) != 1 || len(check.WorkflowOptimization.Proposals) != 1 {
		t.Fatalf("WorkflowOptimization = %#v, want one finding and one proposal", check.WorkflowOptimization)
	}
	proposal := check.WorkflowOptimization.Proposals[0]
	if proposal.TargetPath != "deliverable.merge_method" || !strings.Contains(proposal.SuggestedChange, "squash") {
		t.Fatalf("proposal = %#v, want WORKFLOW.md merge_method squash change", proposal)
	}
}

func TestCheckDoctorRepositoryMergePolicyTargetsRepositoryForDeclaredDrift(t *testing.T) {
	t.Parallel()

	workflow, err := workflowconfig.ParseWorkflow([]byte("---\ntracker:\n  kind: github\n  repository: digitaldrywood/detent\ndeliverable:\n  merge_method: squash\n---\n"))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	check := checkDoctorRepositoryMergePolicy(context.Background(), "detent", globalconfig.Project{}, workflow.Config, doctorDeps{
		githubMergeSettings: func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryMergeSettings, error) {
			return ghconnector.RepositoryMergeSettings{AllowMergeCommit: true, AllowSquashMerge: true}, nil
		},
	})

	if len(check.WorkflowOptimization.Proposals) != 1 {
		t.Fatalf("proposals = %#v, want one", check.WorkflowOptimization.Proposals)
	}
	proposal := check.WorkflowOptimization.Proposals[0]
	if proposal.TargetKind != "repository" || proposal.TargetPath != "digitaldrywood/detent" || !strings.HasPrefix(proposal.SuggestedChange, "gh api --method PATCH") {
		t.Fatalf("proposal = %#v, want repository settings command", proposal)
	}
}

func parseDoctorMergePolicyDeliverable(t *testing.T, mergeMethod string) workflowconfig.Deliverable {
	t.Helper()
	raw := "---\ntracker:\n  kind: github\n"
	if mergeMethod != "" {
		raw += "deliverable:\n  merge_method: " + mergeMethod + "\n"
	}
	workflow, err := workflowconfig.ParseWorkflow([]byte(raw + "---\n"))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	return workflow.Config.Deliverable
}
