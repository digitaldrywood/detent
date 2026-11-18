package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestCheckDoctorCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		configure      func(*workflowconfig.Config)
		sharedPrompt   string
		effortGuidance bool
		wantStates     map[string]doctorCapabilityState
		wantDetails    map[string][]string
	}{
		{
			name: "fully configured project",
			configure: func(cfg *workflowconfig.Config) {
				cfg.Tracker.Kind = workflowconfig.TrackerGitHub
				cfg.Tracker.GitHubStatusSource = workflowconfig.GitHubStatusSourceLabel
				cfg.BacklogAdmission.Enabled = true
				cfg.BacklogAdmission.CriteriaSection = "Admission Criteria"
				cfg.BacklogAdmission.Sources.Untracked = true
				cfg.BacklogAdmission.TargetState = "Todo"
				cfg.BacklogAdmission.ExcludeLabels = []string{"skip-admission"}
				cfg.Agent.Followups.Enabled = true
				cfg.Agent.AutoPromote.Enabled = true
				cfg.Release.Enabled = true
			},
			sharedPrompt:   "## Admission Criteria\n\n- **Alignment** — Prefer operator-visible work.\n",
			effortGuidance: true,
			wantStates: map[string]doctorCapabilityState{
				"agent.followups":            doctorCapabilityConfigured,
				"agent.auto_promote":         doctorCapabilityConfigured,
				"release":                    doctorCapabilityConfigured,
				"detent-agent effort rubric": doctorCapabilityConfigured,
			},
		},
		{
			name: "minimally configured project",
			configure: func(cfg *workflowconfig.Config) {
				cfg.Tracker.Kind = workflowconfig.TrackerGitHub
				cfg.Tracker.GitHubStatusSource = workflowconfig.GitHubStatusSourceLabel
				cfg.Agent.Followups.Enabled = false
			},
			sharedPrompt: "# Workflow\n",
			wantStates: map[string]doctorCapabilityState{
				"agent.followups":            doctorCapabilityUnused,
				"agent.auto_promote":         doctorCapabilityUnused,
				"release":                    doctorCapabilityUnused,
				"detent-agent effort rubric": doctorCapabilityUnused,
			},
			wantDetails: map[string][]string{
				"agent.followups":            {"meaningful out-of-scope discoveries"},
				"agent.auto_promote":         {"review-ready work automatically"},
				"release":                    {"cut releases automatically"},
				"detent-agent effort rubric": {"deliberate reasoning effort"},
			},
		},
		{
			name: "project v2 unavailable capabilities",
			configure: func(cfg *workflowconfig.Config) {
				cfg.Tracker.Kind = workflowconfig.TrackerGitHub
				cfg.Tracker.GitHubStatusSource = workflowconfig.GitHubStatusSourceProjectV2
				cfg.Agent.Followups.Enabled = false
				cfg.BacklogAdmission.CriteriaSection = "Admission Criteria"
				cfg.BacklogAdmission.Sources.States = []string{"Backlog"}
				cfg.BacklogAdmission.TargetState = "Todo"
			},
			sharedPrompt: "## Admission Criteria\n\n- **Alignment** — Prefer operator-visible work.\n",
			wantStates: map[string]doctorCapabilityState{
				"agent.followups":            doctorCapabilityUnused,
				"agent.auto_promote":         doctorCapabilityUnused,
				"release":                    doctorCapabilityUnused,
				"detent-agent effort rubric": doctorCapabilityUnused,
			},
			wantDetails: map[string][]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			if tt.effortGuidance {
				if err := os.WriteFile(filepath.Join(root, "WORKFLOW.md"), []byte("Use a detent-agent effort rubric for every issue.\n"), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}
			cfg := workflowconfig.Default()
			cfg.Workspace.Root = root
			tt.configure(&cfg)
			check := checkDoctorCapabilities(
				globalconfig.Project{ID: "alpha", Workdir: root},
				workflowconfig.Workflow{Config: cfg, SharedPrompt: tt.sharedPrompt},
			)

			if check.Status != doctorOK {
				t.Fatalf("Status = %s, want %s", check.Status, doctorOK)
			}
			if check.Capabilities == nil {
				t.Fatal("Capabilities = nil")
			}
			if len(check.Capabilities.Capabilities) != len(tt.wantStates) {
				t.Fatalf("capabilities len = %d, want %d", len(check.Capabilities.Capabilities), len(tt.wantStates))
			}
			for _, capability := range check.Capabilities.Capabilities {
				wantState, ok := tt.wantStates[capability.Name]
				if !ok {
					t.Errorf("unexpected capability %q", capability.Name)
					continue
				}
				if capability.State != wantState {
					t.Errorf("%s state = %s, want %s", capability.Name, capability.State, wantState)
				}
				for _, wantDetail := range tt.wantDetails[capability.Name] {
					if !strings.Contains(capability.Detail, wantDetail) {
						t.Errorf("%s detail = %q, want containing %q", capability.Name, capability.Detail, wantDetail)
					}
				}
			}

			report := doctorReport{Checks: []doctorCheck{check}, strict: true}
			if report.hasExitFailure() {
				t.Fatal("advisory capability report changed strict doctor exit status")
			}
			var pretty bytes.Buffer
			if err := writeDoctorReport(&pretty, report); err != nil {
				t.Fatalf("writeDoctorReport() error = %v", err)
			}
			for name, state := range tt.wantStates {
				if !strings.Contains(pretty.String(), strings.ToUpper(string(state))+" "+name) {
					t.Errorf("pretty report missing %s %s:\n%s", state, name, pretty.String())
				}
			}
			raw, err := json.Marshal(check)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if !bytes.Contains(raw, []byte(`"capabilities"`)) || !bytes.Contains(raw, []byte(`"items"`)) {
				t.Fatalf("JSON report missing structured capabilities: %s", raw)
			}
		})
	}
}
