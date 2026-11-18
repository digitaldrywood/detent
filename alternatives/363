package doctor

import (
	"os"
	"path/filepath"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestCheckDoctorDisabledBudgetCaps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		budgetYAML string
		wantOK     bool
		wantDetail string
	}{
		{
			name:       "enabled budget does not warn",
			budgetYAML: "  enabled: true\n  per_day_max_usd: 50\n  per_issue_max_usd: 5\n",
		},
		{name: "disabled budget without caps does not warn", budgetYAML: "  enabled: false\n"},
		{
			name:       "disabled daily cap warns",
			budgetYAML: "  per_day_max_usd: 50\n",
			wantOK:     true,
			wantDetail: "budget.enabled=false disables configured caps: budget.per_day_max_usd=50",
		},
		{
			name:       "disabled issue and daily caps warn",
			budgetYAML: "  per_day_max_usd: 646.07\n  per_issue_max_usd: 5\n",
			wantOK:     true,
			wantDetail: "budget.enabled=false disables configured caps: budget.per_day_max_usd=646.07, budget.per_issue_max_usd=5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflow, err := workflowconfig.ParseWorkflow([]byte("---\nbudget:\n" + tt.budgetYAML + "---\n"))
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v", err)
			}
			got, ok := checkDisabledBudgetCaps("pyroapex", workflow.Config.Budget)
			if ok != tt.wantOK {
				t.Fatalf("checkDoctorDisabledBudgetCaps() ok = %t, want %t: %#v", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				return
			}
			if got.Name != "Project pyroapex budget" || got.Status != Warn || got.Detail != tt.wantDetail {
				t.Fatalf("checkDoctorDisabledBudgetCaps() = %#v", got)
			}
			if got.Hint != "Add this exact line under budget: in WORKFLOW.md:\n  enabled: true" {
				t.Fatalf("Hint = %q, want copy-paste enabled line", got.Hint)
			}
		})
	}
}

func TestExpandDoctorWorkspacePath(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}

	got, err := ExpandWorkspacePath("~/repo")
	if err != nil {
		t.Fatalf("expandDoctorWorkspacePath() error = %v", err)
	}
	want := filepath.Join(home, "repo")
	if got != want {
		t.Fatalf("expandDoctorWorkspacePath() = %q, want %q", got, want)
	}
}
