package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestCheckDoctorWorkflowSkillMentions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, shared, local string
		want                []string
	}{
		{name: "skill in shared prompt", shared: "---\nworker:\n  github_token: $TOKEN\n---\nUse `$security-audit` only after a PR exists.\n", want: []string{"WORKFLOW.md:5", "$security-audit"}},
		{name: "skill in local overlay", shared: "Ordinary guidance.\n", local: "First line.\nUse $security-audit after delivery.\n", want: []string{"WORKFLOW.local.md:2", "$security-audit"}},
		{name: "frontmatter only", shared: "---\nworker:\n  github_token: $secret\n---\nOrdinary guidance.\n"},
		{name: "no skill mention", shared: "Use security-audit after delivery.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			definition := workflowconfig.ProjectDefinition{WorkflowPath: filepath.Join(root, "WORKFLOW.md")}
			if err := os.WriteFile(definition.WorkflowPath, []byte(tt.shared), 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.local != "" {
				definition.LocalWorkflowPath = filepath.Join(root, "WORKFLOW.local.md")
				if err := os.WriteFile(definition.LocalWorkflowPath, []byte(tt.local), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			checks := checkDoctorWorkflowSkillMentions("alpha", definition)
			if len(checks) != len(tt.want)/2 {
				t.Fatalf("checks = %#v", checks)
			}
			if len(checks) == 0 {
				return
			}
			if checks[0].Status != doctorWarn || !strings.Contains(checks[0].Detail, tt.want[0]) || !strings.Contains(checks[0].Detail, tt.want[1]) || !strings.Contains(checks[0].Detail, "every session") {
				t.Fatalf("check = %#v", checks[0])
			}
		})
	}
}

func TestDoctorProjectWarnsForWorkflowSkillMention(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "WORKFLOW.md")
	content := "---\ntracker:\n  kind: memory\n---\nUse `$security-audit` after delivery.\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := successfulDoctorDeps()
	deps.loadWorkflow = workflowconfig.LoadWorkflow
	checks := checkDoctorProject(context.Background(), globalconfig.Project{ID: "alpha", Workflow: path, Workdir: root}, deps, RuntimeSecret{}, false)
	for _, check := range checks {
		if check.Name == "Project alpha workflow lint skill invocation" && check.Status == doctorWarn && strings.Contains(check.Detail, path+":5") && strings.Contains(check.Detail, "$security-audit") {
			return
		}
	}
	t.Fatalf("doctor checks omitted skill invocation warning: %#v", checks)
}
