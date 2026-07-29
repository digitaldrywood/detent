package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseWorkflowBacklogAdmission(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
backlog_admission:
  enabled: true
  schedule: "15 4 * * 1"
  sources:
    states: [Backlog]
    labels: [Sentry, sentry]
  target_state: Todo
  criteria_section: Admission criteria
  exclude_labels: [Skip, skip]
  authors:
    allow: ["@octocat"]
  max_candidates_per_run: 20
  max_proposals_per_run: 2
  max_open_proposals: 8
  proposal_expiry_days: 5
  auto_admit: true
  auto_admit_min_confidence: 0.95
---
## Admission criteria

- **Risk** — requires a bounded recovery path.
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Config.Validate() error = %v", err)
	}
	if err := ValidateWorkflowAdmission(workflow); err != nil {
		t.Fatalf("ValidateWorkflowAdmission() error = %v", err)
	}
	got := workflow.Config.BacklogAdmission
	if !got.Enabled || got.Schedule != "15 4 * * 1" || got.TargetState != "Todo" ||
		got.MaxCandidatesPerRun != 20 || got.MaxProposalsPerRun != 2 ||
		got.MaxOpenProposals != 8 || got.ProposalExpiryDays != 5 ||
		!got.AutoAdmit || got.AutoAdmitMinConfidence != 0.95 {
		t.Fatalf("BacklogAdmission = %#v", got)
	}
	if len(got.Sources.Labels) != 1 || got.Sources.Labels[0] != "sentry" ||
		len(got.ExcludeLabels) != 1 || got.ExcludeLabels[0] != "skip" ||
		len(got.Authors.Allow) != 1 || got.Authors.Allow[0] != "octocat" {
		t.Fatalf("normalized filters = sources %#v exclude %#v authors %#v", got.Sources.Labels, got.ExcludeLabels, got.Authors.Allow)
	}
}

func TestBacklogAdmissionCannotUseConfigWithoutWorkflowFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "detent.yaml")
	if err := os.WriteFile(configPath, []byte("schema: 1\ntracker:\n  kind: memory\nbacklog_admission:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(detent.yaml) error = %v", err)
	}
	_, err := LoadProjectDefinition(filepath.Join(dir, "WORKFLOW.md"))
	if err == nil || !strings.Contains(err.Error(), "WORKFLOW.md") || !strings.Contains(err.Error(), "read workflow file") {
		t.Fatalf("LoadProjectDefinition() error = %v, want missing WORKFLOW.md", err)
	}
}

func TestBacklogAdmissionValidate(t *testing.T) {
	t.Parallel()

	valid := BacklogAdmission{
		Enabled:             true,
		Schedule:            "0 6 * * 1-5",
		Sources:             BacklogAdmissionSources{States: []string{"Backlog"}},
		TargetState:         "Todo",
		CriteriaSection:     "Admission criteria",
		MaxCandidatesPerRun: 50,
		MaxProposalsPerRun:  3,
		MaxOpenProposals:    10,
		ProposalExpiryDays:  7,
	}
	states := []string{"Backlog", "Todo", "Done"}
	tests := []struct {
		name    string
		mutate  func(*BacklogAdmission)
		tracker Tracker
		want    string
	}{
		{name: "valid", tracker: Tracker{Kind: TrackerGitHub}},
		{name: "github project v2", tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: GitHubStatusSourceProjectV2}},
		{name: "github issue field", tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: GitHubStatusSourceIssueField}},
		{name: "github label", tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: GitHubStatusSourceLabel}},
		{name: "github local", tracker: Tracker{Kind: TrackerGitHubLocal}},
		{name: "local sqlite", tracker: Tracker{Kind: TrackerLocalSQLite}},
		{name: "memory", tracker: Tracker{Kind: TrackerMemory}},
		{name: "bad cron", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) { cfg.Schedule = "not cron" }, want: "valid five-field cron"},
		{name: "empty sources", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) { cfg.Sources.States = nil }, want: "must configure at least one selector"},
		{name: "unknown source", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) { cfg.Sources.States = []string{"Icebox"} }, want: "sources.states[0] must name"},
		{name: "source equals target", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) { cfg.Sources.States = []string{"Todo"} }, want: "must differ from target_state"},
		{name: "unknown target", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) { cfg.TargetState = "Ready" }, want: "target_state must name"},
		{name: "missing criteria section", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) { cfg.CriteriaSection = "" }, want: "criteria_section is required"},
		{name: "zero candidate cap", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) { cfg.MaxCandidatesPerRun = 0 }, want: "max_candidates_per_run must be greater than 0"},
		{name: "negative proposal cap", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) { cfg.MaxProposalsPerRun = -1 }, want: "max_proposals_per_run must be greater than 0"},
		{name: "zero open cap", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) { cfg.MaxOpenProposals = 0 }, want: "max_open_proposals must be greater than 0"},
		{name: "zero expiry", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) { cfg.ProposalExpiryDays = 0 }, want: "proposal_expiry_days must be greater than 0"},
		{name: "auto admit confidence below range", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) {
			cfg.AutoAdmit = true
			cfg.AutoAdmitMinConfidence = -0.1
		}, want: "auto_admit_min_confidence must be between 0 and 1"},
		{name: "auto admit confidence above range", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) {
			cfg.AutoAdmit = true
			cfg.AutoAdmitMinConfidence = 1.1
		}, want: "auto_admit_min_confidence must be between 0 and 1"},
		{name: "linear", tracker: Tracker{Kind: TrackerLinear}, want: "FetchIssuesByStates is not implemented"},
		{name: "unsupported github source", tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: "milestone"}, want: "tracker.kind github with github_status_source milestone does not declare it"},
		{name: "unsupported tracker", tracker: Tracker{Kind: "gitlab"}, want: "tracker.kind gitlab does not declare it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			got := strings.Join(cfg.Validate("backlog_admission", states, tt.tracker), "; ")
			if tt.want == "" && got != "" {
				t.Fatalf("Validate() = %q, want no problems", got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("Validate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBacklogAdmissionLabelSelectorValidation(t *testing.T) {
	t.Parallel()

	valid := BacklogAdmission{
		Enabled:             true,
		Schedule:            "0 6 * * 1-5",
		Sources:             BacklogAdmissionSources{Labels: []string{"sentry"}},
		TargetState:         "Todo",
		CriteriaSection:     "Admission criteria",
		MaxCandidatesPerRun: 50,
		MaxProposalsPerRun:  3,
		MaxOpenProposals:    10,
		ProposalExpiryDays:  7,
	}
	states := []string{"Backlog", "Todo", "Blocked", "Done"}
	tests := []struct {
		name    string
		tracker Tracker
		mutate  func(*BacklogAdmission)
		want    string
	}{
		{name: "github label", tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: GitHubStatusSourceLabel}},
		{name: "github issue field", tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: GitHubStatusSourceIssueField}},
		{name: "github local", tracker: Tracker{Kind: TrackerGitHubLocal}},
		{name: "local sqlite", tracker: Tracker{Kind: TrackerLocalSQLite}},
		{name: "memory", tracker: Tracker{Kind: TrackerMemory}},
		{name: "github project v2", tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: GitHubStatusSourceProjectV2}, want: "does not declare complete label reads"},
		{name: "linear", tracker: Tracker{Kind: TrackerLinear}, want: "does not declare complete label reads"},
		{
			name:    "configured status prefix",
			tracker: Tracker{Kind: TrackerMemory, StatusLabelPrefix: "workflow/"},
			mutate: func(cfg *BacklogAdmission) {
				cfg.Sources.Labels = []string{"Workflow/Ready"}
			},
			want: `must not use status label prefix "workflow/"`,
		},
		{
			name:    "project v2 excluded labels",
			tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: GitHubStatusSourceProjectV2},
			mutate: func(cfg *BacklogAdmission) {
				cfg.Sources.Labels = nil
				cfg.Sources.States = []string{"Backlog"}
				cfg.ExcludeLabels = []string{"skip"}
			},
			want: "fetches only the first 20 labels",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			got := strings.Join(cfg.Validate("backlog_admission", states, test.tracker), "; ")
			if test.want == "" && got != "" {
				t.Fatalf("Validate() = %q, want no problems", got)
			}
			if test.want != "" && !strings.Contains(got, test.want) {
				t.Fatalf("Validate() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBacklogAdmissionAutoAdmitDefaultsOff(t *testing.T) {
	t.Parallel()

	cfg := Default().BacklogAdmission
	if cfg.AutoAdmit {
		t.Fatal("BacklogAdmission.AutoAdmit = true, want false")
	}
	if cfg.AutoAdmitMinConfidence != DefaultBacklogAdmissionAutoAdmitMinConfidence {
		t.Fatalf(
			"BacklogAdmission.AutoAdmitMinConfidence = %v, want %v",
			cfg.AutoAdmitMinConfidence,
			DefaultBacklogAdmissionAutoAdmitMinConfidence,
		)
	}
}

func TestResolveAdmissionCriteriaHeadingFormsAndDimensions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		prompt     string
		section    string
		wantText   string
		dimensions []string
	}{
		{
			name:       "ATX is case insensitive at any level and includes nested content",
			prompt:     "# Workflow\n\n### ADMISSION CRITERIA ###\n\n#### Incident severity\n\nEscalate customer-visible outages.\n\n#### Recovery path\n\nRequire an operator action.\n\n### Later\n\nIgnored.",
			section:    "admission criteria",
			wantText:   "#### Incident severity",
			dimensions: []string{"Incident severity", "Recovery path"},
		},
		{
			name:       "Setext section with bold dimensions",
			prompt:     "Admission criteria\n==================\n\n- **Evidence** — cites a reproducible signal.\n- **Urgency**: requires action now.\n\nElsewhere\n=========\n\nIgnored.",
			section:    "ADMISSION CRITERIA",
			wantText:   "**Evidence**",
			dimensions: []string{"Evidence", "Urgency"},
		},
		{
			name:       "fenced examples are not headings or dimensions",
			prompt:     "# Workflow\n\n```markdown\n## Admission criteria\n\n- **Example** — ignored.\n```\n\n## Admission criteria\n\n- **Risk** — requires a bounded recovery path.",
			section:    "Admission criteria",
			wantText:   "**Risk**",
			dimensions: []string{"Risk"},
		},
		{
			name:       "literal trailing hash is preserved",
			prompt:     "## Admission C#\n\n- **Runtime** — applies to managed code.",
			section:    "Admission C#",
			wantText:   "**Runtime**",
			dimensions: []string{"Runtime"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveAdmissionCriteria(tt.prompt, tt.section)
			if err != nil {
				t.Fatalf("ResolveAdmissionCriteria() error = %v", err)
			}
			if !strings.Contains(got.Text, tt.wantText) || strings.Contains(got.Text, "Ignored.") {
				t.Fatalf("criteria text = %q", got.Text)
			}
			if len(got.Dimensions) != len(tt.dimensions) {
				t.Fatalf("dimensions = %#v, want %#v", got.Dimensions, tt.dimensions)
			}
			for index, want := range tt.dimensions {
				if got.Dimensions[index].Name != want {
					t.Fatalf("Dimensions[%d].Name = %q, want %q", index, got.Dimensions[index].Name, want)
				}
			}
		})
	}
}

func TestResolveAdmissionCriteriaFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prompt string
		want   string
	}{
		{name: "missing", prompt: "# Workflow", want: "was not found"},
		{name: "duplicate across levels", prompt: "## Admission criteria\n\n- **One** — A.\n\n### Admission Criteria\n\n- **Two** — B.", want: "duplicated"},
		{name: "empty", prompt: "## Admission criteria\n\n## Next", want: "is empty"},
		{name: "no dimensions", prompt: "## Admission criteria\n\nUse good judgment.", want: "must define at least one dimension"},
		{name: "duplicate dimensions", prompt: "## Admission criteria\n\n- **Risk** — A.\n- **risk** — B.", want: "dimension \"risk\" is duplicated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveAdmissionCriteria(tt.prompt, "Admission criteria")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ResolveAdmissionCriteria() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestAdmissionCriteriaNeverUsesLocalWorkflowOverlay(t *testing.T) {
	t.Parallel()

	shared := []byte("---\ntracker:\n  kind: memory\n---\n# Workflow\n\n## Admission criteria\n\n- **Shared** — repository-owned text.\n")
	local := []byte("---\n---\n## Admission criteria\n\n- **Local** — machine-only text.\n")
	workflow, err := ParseWorkflowOverlay(shared, local, "WORKFLOW.local.md")
	if err != nil {
		t.Fatalf("ParseWorkflowOverlay() error = %v", err)
	}
	if !strings.Contains(workflow.Prompt, "**Local**") {
		t.Fatalf("merged Prompt = %q, want local overlay", workflow.Prompt)
	}
	criteria, err := ResolveAdmissionCriteria(workflow.SharedPrompt, "Admission criteria")
	if err != nil {
		t.Fatalf("ResolveAdmissionCriteria() error = %v", err)
	}
	if len(criteria.Dimensions) != 1 || criteria.Dimensions[0].Name != "Shared" || strings.Contains(criteria.Text, "Local") {
		t.Fatalf("criteria = %#v, want shared-only dimension", criteria)
	}
}

func TestBacklogAdmissionWarnings(t *testing.T) {
	t.Parallel()

	cfg := BacklogAdmission{
		Enabled:       true,
		ExcludeLabels: []string{"skip"},
		Authors:       BacklogAdmissionAuthors{Allow: []string{"octocat"}},
	}
	localWarnings := strings.Join(BacklogAdmissionWarnings(cfg, Tracker{Kind: TrackerLocalSQLite}), "; ")
	if !strings.Contains(localWarnings, "does not discover authors") {
		t.Fatalf("local warnings = %q", localWarnings)
	}
	memoryWarnings := strings.Join(BacklogAdmissionWarnings(cfg, Tracker{Kind: TrackerMemory}), "; ")
	if !strings.Contains(memoryWarnings, "evaluation-only across restarts") {
		t.Fatalf("memory warnings = %q", memoryWarnings)
	}
}
