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
  kind: github
  api_key: token
  github_status_source: issue_field
  repository: digitaldrywood/detent
backlog_admission:
  enabled: true
  schedule: "15 4 * * 1"
  sources:
    states: [Backlog]
    labels: [Sentry, sentry]
    untracked: false
  target_state: Todo
  criteria_section: Admission criteria
  require_effort: true
  effort_file: WORKFLOW.md
  effort_section: Issue effort selection
  exclude_labels: [Skip, skip]
  authors:
    allow: ["@octocat"]
    allow_association: [member, MEMBER, collaborator]
  max_candidates_per_run: 20
  max_proposals_per_run: 2
  max_open_proposals: 8
  proposal_expiry_days: 5
  auto_admit: true
  auto_admit_by_label:
    Defect: true
    Requires-Human-Review: false
  auto_admit_min_confidence: 0.95
---
## Admission criteria

- **Risk** — requires a bounded recovery path.

## Issue effort selection

- **medium** — small and mechanical.
- **high** — standard feature work.
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
		!got.AutoAdmit || got.AutoAdmitMinConfidence != 0.95 || got.Sources.Untracked ||
		!got.RequireEffort || got.EffortFile != BacklogAdmissionEffortFileWorkflow || got.EffortSection != "Issue effort selection" {
		t.Fatalf("BacklogAdmission = %#v", got)
	}
	if len(got.AutoAdmitByLabel) != 2 || !got.AutoAdmitByLabel["defect"] || got.AutoAdmitByLabel["requires-human-review"] {
		t.Fatalf("AutoAdmitByLabel = %#v", got.AutoAdmitByLabel)
	}
	if len(got.Sources.Labels) != 1 || got.Sources.Labels[0] != "sentry" ||
		len(got.ExcludeLabels) != 1 || got.ExcludeLabels[0] != "skip" ||
		len(got.Authors.Allow) != 1 || got.Authors.Allow[0] != "octocat" ||
		len(got.Authors.AllowAssociation) != 2 ||
		got.Authors.AllowAssociation[0] != "MEMBER" ||
		got.Authors.AllowAssociation[1] != "COLLABORATOR" {
		t.Fatalf("normalized filters = sources %#v exclude %#v authors %#v", got.Sources, got.ExcludeLabels, got.Authors)
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
		{name: "missing effort section", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) {
			cfg.RequireEffort = true
		}, want: "effort_section is required"},
		{name: "invalid effort file", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) {
			cfg.RequireEffort = true
			cfg.EffortFile = "README.md"
			cfg.EffortSection = "Effort"
		}, want: "effort_file must be WORKFLOW.md or AGENTS.md"},
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
		{name: "label auto admit confidence outside range", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) {
			cfg.AutoAdmitByLabel = map[string]bool{"defect": true}
			cfg.AutoAdmitMinConfidence = -0.1
		}, want: "auto_admit_min_confidence must be between 0 and 1"},
		{name: "blank auto admit label", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) {
			cfg.AutoAdmitByLabel = map[string]bool{" ": false}
		}, want: "auto_admit_by_label labels must not be blank"},
		{name: "project v2 label policy", tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: GitHubStatusSourceProjectV2}, mutate: func(cfg *BacklogAdmission) {
			cfg.AutoAdmitByLabel = map[string]bool{"feature": false}
		}, want: "auto_admit_by_label requires complete issue labels"},
		{name: "linear", tracker: Tracker{Kind: TrackerLinear}, want: "FetchIssuesByStates is not implemented"},
		{name: "unsupported github source", tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: "milestone"}, want: "tracker.kind github with github_status_source milestone does not declare it"},
		{name: "unsupported tracker", tracker: Tracker{Kind: "gitlab"}, want: "tracker.kind gitlab does not declare it"},
		{name: "invalid association", tracker: Tracker{Kind: TrackerGitHub}, mutate: func(cfg *BacklogAdmission) {
			cfg.Authors.AllowAssociation = []string{"maintainer"}
		}, want: "must be one of OWNER"},
		{name: "association local sqlite", tracker: Tracker{Kind: TrackerLocalSQLite}, mutate: func(cfg *BacklogAdmission) {
			cfg.Authors.AllowAssociation = []string{"member"}
		}, want: "tracker.kind local_sqlite cannot supply it"},
		{name: "association memory", tracker: Tracker{Kind: TrackerMemory}, mutate: func(cfg *BacklogAdmission) {
			cfg.Authors.AllowAssociation = []string{"member"}
		}, want: "tracker.kind memory cannot supply it"},
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

func TestBacklogAdmissionAutoAdmitForLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fallback bool
		policies map[string]bool
		labels   []string
		want     bool
	}{
		{name: "defect override admits", policies: map[string]bool{"defect": true}, labels: []string{"Defect"}, want: true},
		{name: "feature override holds", fallback: true, policies: map[string]bool{"feature": false}, labels: []string{"feature"}},
		{name: "propose-only wins over admit class", fallback: true, policies: map[string]bool{"defect": true, "feature": false}, labels: []string{"defect", "feature"}},
		{name: "unknown label uses enabled default", fallback: true, policies: map[string]bool{"defect": true}, labels: []string{"docs"}, want: true},
		{name: "unknown label uses disabled default", policies: map[string]bool{"defect": true}, labels: []string{"docs"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := BacklogAdmission{AutoAdmit: tt.fallback, AutoAdmitByLabel: tt.policies}
			cfg.Normalize()
			if got := cfg.AutoAdmitForLabels(tt.labels); got != tt.want {
				t.Fatalf("AutoAdmitForLabels(%#v) = %t, want %t", tt.labels, got, tt.want)
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
	if cfg.RequireEffort || cfg.EffortSection != "" {
		t.Fatalf("effort defaults = require:%t section:%q, want disabled", cfg.RequireEffort, cfg.EffortSection)
	}
	if cfg.AutoAdmitMinConfidence != DefaultBacklogAdmissionAutoAdmitMinConfidence {
		t.Fatalf(
			"BacklogAdmission.AutoAdmitMinConfidence = %v, want %v",
			cfg.AutoAdmitMinConfidence,
			DefaultBacklogAdmissionAutoAdmitMinConfidence,
		)
	}
}

func TestBacklogAdmissionScheduleNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		schedule string
		want     string
	}{
		{name: "default", want: DefaultBacklogAdmissionSchedule},
		{name: "explicit", schedule: "0 6 * * 1-5", want: "0 6 * * 1-5"},
		{name: "trim explicit", schedule: "  0 * * * *  ", want: "0 * * * *"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := BacklogAdmission{Schedule: tt.schedule}
			cfg.Normalize()
			if cfg.Schedule != tt.want {
				t.Fatalf("Schedule = %q, want %q", cfg.Schedule, tt.want)
			}
		})
	}
}

func TestBacklogAdmissionValidateUntrackedSelector(t *testing.T) {
	t.Parallel()

	valid := BacklogAdmission{
		Enabled:             true,
		Schedule:            "0 6 * * 1-5",
		Sources:             BacklogAdmissionSources{Untracked: true},
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
		tracker Tracker
		want    string
	}{
		{
			name:    "github label",
			tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: GitHubStatusSourceLabel},
		},
		{
			name:    "github issue field",
			tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: GitHubStatusSourceIssueField},
			want:    "untracked issues are only defined for github label status",
		},
		{
			name:    "github project v2",
			tracker: Tracker{Kind: TrackerGitHub, GitHubStatusSource: GitHubStatusSourceProjectV2},
			want:    "untracked issues are only defined for github label status",
		},
		{
			name:    "github local",
			tracker: Tracker{Kind: TrackerGitHubLocal},
			want:    "github_local status drift does not populate UntrackedOpen",
		},
		{
			name:    "linear",
			tracker: Tracker{Kind: TrackerLinear},
			want:    "does not provide github label status drift",
		},
		{
			name:    "memory",
			tracker: Tracker{Kind: TrackerMemory},
			want:    "does not provide github label status drift",
		},
		{
			name:    "local sqlite",
			tracker: Tracker{Kind: TrackerLocalSQLite},
			want:    "does not provide github label status drift",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := strings.Join(valid.Validate("backlog_admission", states, tt.tracker), "; ")
			if tt.want == "" && got != "" {
				t.Fatalf("Validate() = %q, want no problems", got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("Validate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBacklogAdmissionAuthorWarnings(t *testing.T) {
	t.Parallel()

	statesOnly := BacklogAdmission{
		Enabled: true,
		Sources: BacklogAdmissionSources{States: []string{"Backlog"}},
	}
	if warning := BacklogAdmissionPublicExposureWarning(statesOnly, "public"); !strings.Contains(warning, "untrusted issue authors") {
		t.Fatalf("states-only public warning = %q", warning)
	}
	restricted := statesOnly
	restricted.Authors = BacklogAdmissionAuthors{AllowAssociation: []string{"OWNER", "MEMBER", "COLLABORATOR"}}
	if warning := BacklogAdmissionPublicExposureWarning(restricted, "public"); warning != "" {
		t.Fatalf("trusted association warning = %q", warning)
	}
	restricted.Authors.AllowAssociation = append(restricted.Authors.AllowAssociation, "CONTRIBUTOR")
	if warning := BacklogAdmissionPublicExposureWarning(restricted, "public"); warning == "" {
		t.Fatal("contributor association warning is empty")
	}
	if warning := BacklogAdmissionPublicExposureWarning(statesOnly, "private"); warning != "" {
		t.Fatalf("private warning = %q", warning)
	}

	authors := BacklogAdmissionAuthors{AllowAssociation: []string{"MEMBER"}}
	if warning := BacklogAdmissionBotExclusionWarning(authors, true); !strings.Contains(warning, "integration accounts") {
		t.Fatalf("bot exclusion warning = %q", warning)
	}
	untracked := statesOnly
	untracked.Sources.Untracked = true
	untracked.Authors = authors
	if warnings := strings.Join(BacklogAdmissionWarnings(untracked, Tracker{Kind: TrackerGitHub}), "; "); !strings.Contains(warnings, "integration accounts") {
		t.Fatalf("BacklogAdmissionWarnings() = %q", warnings)
	}
	authors.Allow = []string{"dependabot[bot]"}
	if warning := BacklogAdmissionBotExclusionWarning(authors, true); warning != "" {
		t.Fatalf("allowlisted bot warning = %q", warning)
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

func TestResolveAdmissionEffortRubric(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prompt  string
		section string
		want    []string
		wantErr string
	}{
		{
			name:    "reads code and bold list items",
			prompt:  "## Issue effort selection\n\n- `medium` — small and mechanical.\n- **high** — standard feature work.\n- `xhigh`: tricky state semantics.\n\n## Later\n\nIgnored.",
			section: "issue effort selection",
			want:    []string{"medium", "high", "xhigh"},
		},
		{
			name:    "ignores fenced examples",
			prompt:  "## Effort\n\n```markdown\n- `low` — ignored.\n```\n\n- `high` — selected.",
			section: "Effort",
			want:    []string{"high"},
		},
		{
			name:    "rejects duplicate values",
			prompt:  "## Effort\n\n- `high` — standard.\n- **HIGH** — duplicate.",
			section: "Effort",
			wantErr: "duplicated",
		},
		{
			name:    "requires formatted values",
			prompt:  "## Effort\n\nChoose a suitable effort.",
			section: "Effort",
			wantErr: "must define at least one effort",
		},
		{
			name:    "rejects unsupported value characters",
			prompt:  "## Effort\n\n- `very high` — not safe for an override block.",
			section: "Effort",
			wantErr: "contains unsupported characters",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveAdmissionEffortRubric(tt.prompt, tt.section)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveAdmissionEffortRubric() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveAdmissionEffortRubric() error = %v", err)
			}
			if strings.Contains(got.Text, "Ignored.") || len(got.Efforts) != len(tt.want) {
				t.Fatalf("rubric = %#v, want efforts %#v", got, tt.want)
			}
			for index, want := range tt.want {
				if got.Efforts[index] != want {
					t.Fatalf("Efforts[%d] = %q, want %q", index, got.Efforts[index], want)
				}
			}
		})
	}
}

func TestProjectDefinitionResolvesAdmissionEffortFromConfiguredFile(t *testing.T) {
	t.Parallel()

	config := []byte(`schema: 1
tracker:
  kind: memory
backlog_admission:
  enabled: true
  sources:
    states: [Backlog]
  target_state: Todo
  criteria_section: Admission Criteria
  require_effort: true
  effort_file: AGENTS.md
  effort_section: Issue Effort Selection
`)
	workflowPrompt := []byte("## Admission Criteria\n\n- **Alignment** — ready.\n")
	agentsPrompt := []byte("## Issue Effort Selection\n\n- `medium` — small.\n- `high` — standard.\n")

	workflow, err := ParseProjectDefinition(ProjectDefinitionSources{
		WorkflowPath: "WORKFLOW.md",
		Workflow:     workflowPrompt,
		ConfigPath:   "detent.yaml",
		Config:       config,
		HasConfig:    true,
		AgentsPath:   "AGENTS.md",
		Agents:       agentsPrompt,
		HasAgents:    true,
	})
	if err != nil {
		t.Fatalf("ParseProjectDefinition() error = %v", err)
	}
	rubric, err := ResolveWorkflowAdmissionEffortRubric(workflow)
	if err != nil {
		t.Fatalf("ResolveWorkflowAdmissionEffortRubric() error = %v", err)
	}
	if rubric.Section != "Issue Effort Selection" || len(rubric.Efforts) != 2 || rubric.Efforts[0] != "medium" || rubric.Efforts[1] != "high" {
		t.Fatalf("rubric = %#v, want AGENTS.md efforts", rubric)
	}
}

func TestProjectDefinitionReportsAdmissionEffortInDifferentFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		effortFile string
		workflow   string
		agents     string
		want       string
	}{
		{
			name:       "configured agents but section is in workflow",
			effortFile: BacklogAdmissionEffortFileAgents,
			workflow:   "## Admission Criteria\n\n- **Alignment** — ready.\n\n## Issue Effort Selection\n\n- `high` — standard.\n",
			agents:     "# Agent guidance\n",
			want:       "exists in WORKFLOW.md instead; set backlog_admission.effort_file to WORKFLOW.md",
		},
		{
			name:       "configured workflow but section is in agents",
			effortFile: BacklogAdmissionEffortFileWorkflow,
			workflow:   "## Admission Criteria\n\n- **Alignment** — ready.\n",
			agents:     "## Issue Effort Selection\n\n- `high` — standard.\n",
			want:       "exists in AGENTS.md instead; set backlog_admission.effort_file to AGENTS.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := []byte("schema: 1\ntracker:\n  kind: memory\nbacklog_admission:\n  enabled: true\n  sources:\n    states: [Backlog]\n  target_state: Todo\n  criteria_section: Admission Criteria\n  require_effort: true\n  effort_file: " + tt.effortFile + "\n  effort_section: Issue Effort Selection\n")
			_, err := ParseProjectDefinition(ProjectDefinitionSources{
				WorkflowPath: "WORKFLOW.md",
				Workflow:     []byte(tt.workflow),
				ConfigPath:   "detent.yaml",
				Config:       config,
				HasConfig:    true,
				AgentsPath:   "AGENTS.md",
				Agents:       []byte(tt.agents),
				HasAgents:    true,
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParseProjectDefinition() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestProjectDefinitionHashesConfiguredAdmissionEffortFile(t *testing.T) {
	t.Parallel()

	parse := func(t *testing.T, effortFile string, agents string) Workflow {
		t.Helper()
		workflowPrompt := "## Admission Criteria\n\n- **Alignment** — ready.\n"
		if effortFile == BacklogAdmissionEffortFileWorkflow {
			workflowPrompt += "\n## Issue Effort Selection\n\n- `high` — standard.\n"
		}
		config := []byte("schema: 1\ntracker:\n  kind: memory\nbacklog_admission:\n  enabled: true\n  sources:\n    states: [Backlog]\n  target_state: Todo\n  criteria_section: Admission Criteria\n  require_effort: true\n  effort_file: " + effortFile + "\n  effort_section: Issue Effort Selection\n")
		workflow, err := ParseProjectDefinition(ProjectDefinitionSources{
			WorkflowPath: "WORKFLOW.md",
			Workflow:     []byte(workflowPrompt),
			ConfigPath:   "detent.yaml",
			Config:       config,
			HasConfig:    true,
			AgentsPath:   "AGENTS.md",
			Agents:       []byte(agents),
			HasAgents:    true,
		})
		if err != nil {
			t.Fatalf("ParseProjectDefinition() error = %v", err)
		}
		return workflow
	}

	agentsA := "## Issue Effort Selection\n\n- `high` — standard.\n"
	agentsB := "## Issue Effort Selection\n\n- `high` — cross-cutting.\n"
	fromAgentsA := parse(t, BacklogAdmissionEffortFileAgents, agentsA)
	fromAgentsB := parse(t, BacklogAdmissionEffortFileAgents, agentsB)
	if fromAgentsA.SourceHash == fromAgentsB.SourceHash {
		t.Fatal("AGENTS.md-backed source hash did not change with the configured rubric")
	}
	fromWorkflowA := parse(t, BacklogAdmissionEffortFileWorkflow, agentsA)
	fromWorkflowB := parse(t, BacklogAdmissionEffortFileWorkflow, agentsB)
	if fromWorkflowA.SourceHash != fromWorkflowB.SourceHash {
		t.Fatal("WORKFLOW.md-backed source hash changed for unrelated AGENTS.md guidance")
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
