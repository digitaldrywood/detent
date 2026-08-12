package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestInspectOnboardingMergePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		sharedMergeMethod    string
		localMergeMethod     string
		settings             ghconnector.RepositoryMergeSettings
		wantMethod           string
		wantSource           string
		wantSelectedEnabled  bool
		wantAdditionalEnable bool
	}{
		{
			name:                "template default",
			settings:            ghconnector.RepositoryMergeSettings{AllowSquashMerge: true},
			wantMethod:          workflowconfig.MergeMethodSquash,
			wantSource:          "template_default",
			wantSelectedEnabled: true,
		},
		{
			name:                 "effective local override",
			sharedMergeMethod:    workflowconfig.MergeMethodSquash,
			localMergeMethod:     workflowconfig.MergeMethodRebase,
			settings:             ghconnector.RepositoryMergeSettings{AllowMergeCommit: true, AllowRebaseMerge: true},
			wantMethod:           workflowconfig.MergeMethodRebase,
			wantSource:           "effective_project_definition",
			wantSelectedEnabled:  true,
			wantAdditionalEnable: true,
		},
		{
			name:                 "configured method forbidden",
			sharedMergeMethod:    workflowconfig.MergeMethodMerge,
			settings:             ghconnector.RepositoryMergeSettings{AllowSquashMerge: true},
			wantMethod:           workflowconfig.MergeMethodMerge,
			wantSource:           "effective_project_definition",
			wantAdditionalEnable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tt.sharedMergeMethod != "" {
				writeOnboardingMergePolicyDefinition(t, root, tt.sharedMergeMethod, tt.localMergeMethod)
			}

			var effectiveMethod string
			result, err := inspectOnboardingMergePolicy(t.Context(), onboardingMergePolicyInspectionConfig{
				SourceRoot: root,
				Repository: "digitaldrywood/detent",
			}, onboardingMergePolicyInspectionDeps{
				loadWorkflow: workflowconfig.LoadWorkflow,
				githubMergeSettings: func(_ context.Context, cfg workflowconfig.Config, repository string) (ghconnector.RepositoryMergeSettings, error) {
					if repository != "digitaldrywood/detent" {
						t.Fatalf("repository = %q, want digitaldrywood/detent", repository)
					}
					effectiveMethod = cfg.Deliverable.EffectiveMergeMethod()
					return tt.settings, nil
				},
			})
			if err != nil {
				t.Fatalf("inspectOnboardingMergePolicy() error = %v", err)
			}
			if result.SelectedMergeMethod != tt.wantMethod || effectiveMethod != tt.wantMethod {
				t.Fatalf("selected methods = result %q, reader %q, want %q", result.SelectedMergeMethod, effectiveMethod, tt.wantMethod)
			}
			if result.SelectionSource != tt.wantSource {
				t.Fatalf("SelectionSource = %q, want %q", result.SelectionSource, tt.wantSource)
			}
			if result.SelectedMethodEnabled != tt.wantSelectedEnabled {
				t.Fatalf("SelectedMethodEnabled = %t, want %t", result.SelectedMethodEnabled, tt.wantSelectedEnabled)
			}
			if result.AdditionalMethodsEnabled != tt.wantAdditionalEnable {
				t.Fatalf("AdditionalMethodsEnabled = %t, want %t", result.AdditionalMethodsEnabled, tt.wantAdditionalEnable)
			}
			if result.AllowMergeCommit != tt.settings.AllowMergeCommit ||
				result.AllowSquashMerge != tt.settings.AllowSquashMerge ||
				result.AllowRebaseMerge != tt.settings.AllowRebaseMerge {
				t.Fatalf("repository settings = %#v, want %#v", result, tt.settings)
			}
		})
	}
}

func writeOnboardingMergePolicyDefinition(t *testing.T, root string, sharedMethod string, localMethod string) {
	t.Helper()
	files := map[string]string{
		"WORKFLOW.md": "Shared direction.\n",
		"detent.yaml": "schema: 1\ntracker:\n  kind: memory\ndeliverable:\n  merge_method: " + sharedMethod + "\n",
	}
	if localMethod != "" {
		files["detent.local.yaml"] = "schema: 1\ndeliverable:\n  merge_method: " + localMethod + "\n"
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
}
