package cli

import (
	"strings"
	"testing"
)

func TestCheckDoctorWorkflowPromptSize(t *testing.T) {
	t.Parallel()

	oversizedPrompt := strings.Join([]string{
		"## Operating Rules",
		strings.Repeat("Keep the core contract concise. ", 40),
		"## GitHub CI Recovery",
		strings.Repeat("Use gh to inspect the pull request and GitHub check-runs before retrying CI. ", 20),
		"## Database Migration Runbook",
		strings.Repeat("Run the SQL migration against the database and verify rollback behavior. ", 20),
	}, "\n\n")
	tests := []struct {
		name            string
		workflowPath    string
		prompt          string
		skillsPath      string
		threshold       int
		wantWarning     bool
		wantSkillsPath  string
		wantSuggestions []string
	}{
		{
			name:         "under threshold",
			workflowPath: "/repo/WORKFLOW.md",
			prompt:       "## Operating Rules\n\nKeep the issue scoped and run the validation gate.",
			threshold:    100,
		},
		{
			name:         "empty workflow",
			workflowPath: "/repo/WORKFLOW.md",
			threshold:    1,
		},
		{
			name:         "missing workflow prompt",
			workflowPath: "/repo/missing/WORKFLOW.md",
			prompt:       " \n",
			threshold:    1,
		},
		{
			name:            "over threshold with section suggestions",
			workflowPath:    "/repo/WORKFLOW.md",
			threshold:       100,
			prompt:          oversizedPrompt,
			wantWarning:     true,
			wantSkillsPath:  ".detent/skills",
			wantSuggestions: []string{"github-ci-recovery", "database-migration-runbook"},
		},
		{
			name:            "configured skills path",
			workflowPath:    "/repo/WORKFLOW.md",
			prompt:          oversizedPrompt,
			skillsPath:      "ops/agent-skills",
			threshold:       100,
			wantWarning:     true,
			wantSkillsPath:  "ops/agent-skills",
			wantSuggestions: []string{"github-ci-recovery", "database-migration-runbook"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			check, warned := checkDoctorWorkflowPromptSize("alpha", tt.workflowPath, tt.prompt, tt.skillsPath, tt.threshold)
			if warned != tt.wantWarning {
				t.Fatalf("warned = %t, want %t; check = %#v", warned, tt.wantWarning, check)
			}
			if !tt.wantWarning {
				return
			}
			if check.Status != doctorWarn || !strings.Contains(check.Name, "workflow lint prompt size") {
				t.Fatalf("check = %#v, want workflow lint warning", check)
			}
			if !strings.Contains(check.Detail, "above the 100-token threshold") || !strings.Contains(check.Hint, "existing skill-creation flow") || !strings.Contains(check.Hint, tt.wantSkillsPath) {
				t.Fatalf("check = %#v, want threshold and skill-creation guidance", check)
			}
			got := make(map[string]bool, len(check.WorkflowSkillSuggestions))
			for _, suggestion := range check.WorkflowSkillSuggestions {
				got[suggestion.SkillName] = true
				if suggestion.Heading == "" || suggestion.EstimatedTokens <= 0 || !strings.HasPrefix(suggestion.Path, tt.wantSkillsPath+"/") {
					t.Fatalf("suggestion = %#v, want heading and token estimate", suggestion)
				}
			}
			for _, want := range tt.wantSuggestions {
				if !got[want] || !strings.Contains(check.Detail, tt.wantSkillsPath+"/"+want+".md") {
					t.Fatalf("suggestions = %#v, detail = %q, want %q", check.WorkflowSkillSuggestions, check.Detail, want)
				}
			}
		})
	}
}

func TestDoctorWorkflowTokenThresholdFlag(t *testing.T) {
	t.Parallel()

	configPath := ""
	env := ""
	logLevel := ""
	host := ""
	port := -1
	cmd := newDoctorCommand(&configPath, &env, &logLevel, &host, &port, options{})
	flag := cmd.Flags().Lookup("workflow-token-threshold")
	if flag == nil {
		t.Fatal("workflow-token-threshold flag is missing")
	}
	if flag.DefValue != "4000" {
		t.Fatalf("workflow-token-threshold default = %q, want 4000", flag.DefValue)
	}
}
