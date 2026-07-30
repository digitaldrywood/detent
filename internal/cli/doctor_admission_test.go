package cli

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestDoctorAdmissionDiagnostics(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	if _, err := db.ExecContext(ctx, `
CREATE TABLE backlog_admission_runs (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  started_at TEXT NOT NULL,
  completed_at TEXT NOT NULL,
  outcome TEXT NOT NULL,
  deferred_reason TEXT,
  candidates_found_count INTEGER NOT NULL,
  candidates_count INTEGER NOT NULL,
  proposed_count INTEGER NOT NULL,
  skipped_json TEXT NOT NULL,
  truncated_json TEXT NOT NULL,
  issues_json TEXT NOT NULL,
  error TEXT
);
INSERT INTO backlog_admission_runs (
  project_id, started_at, completed_at, outcome, deferred_reason, candidates_found_count,
  candidates_count, proposed_count, skipped_json, truncated_json, issues_json, error
) VALUES
  ('detent', '2026-07-29T10:02:30Z', '2026-07-29T10:03:00Z', 'failed', NULL, 12, 4, 1, '{"excluded_label":2}', '{"candidate_cap":8}', '[{"identifier":"digitaldrywood/detent#1535"}]', 'third failure'),
  ('detent', '2026-07-29T10:01:40Z', '2026-07-29T10:02:00Z', 'failed', NULL, 10, 4, 0, '{}', '{"candidate_cap":6}', '[]', 'second failure'),
  ('detent', '2026-07-29T10:00:50Z', '2026-07-29T10:01:00Z', 'failed', NULL, 8, 4, 0, '{}', '{"candidate_cap":4}', '[]', 'first failure');
CREATE TABLE codex_sessions (
  project_id TEXT,
  identifier TEXT,
  completed_at TEXT,
  runtime_seconds INTEGER NOT NULL,
  total_tokens INTEGER NOT NULL
);
INSERT INTO codex_sessions (
  project_id, identifier, completed_at, runtime_seconds, total_tokens
) VALUES
  ('detent', 'detent/admission', '2026-07-29T10:02:55Z', 25, 36000),
  ('detent', 'detent/admission', '2026-07-29T10:01:55Z', 35, 40000),
  ('detent', 'digitaldrywood/detent#1535', '2026-07-29T11:00:00Z', 900, 2000000);
CREATE TABLE backlog_admission_proposals (
  project_id TEXT NOT NULL,
  status TEXT NOT NULL,
  decision_seconds INTEGER
);
INSERT INTO backlog_admission_proposals (project_id, status, decision_seconds) VALUES
  ('detent', 'accepted', 60),
  ('detent', 'expired', 604800);
CREATE TABLE backlog_admission_downstream_outcomes (
  project_id TEXT NOT NULL,
  completed_at TEXT,
  rework_count INTEGER NOT NULL,
  review_churn_count INTEGER NOT NULL,
  spend_usd REAL NOT NULL
);
INSERT INTO backlog_admission_downstream_outcomes (
  project_id, completed_at, rework_count, review_churn_count, spend_usd
) VALUES ('detent', '2026-07-29T11:00:00Z', 2, 3, 4.5);
CREATE TABLE workflow_phase_events (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  phase_type TEXT NOT NULL,
  status TEXT NOT NULL,
  metadata_json TEXT NOT NULL
);
INSERT INTO workflow_phase_events (project_id, phase_type, status, metadata_json) VALUES
  ('detent', 'lane', 'entered', '{"provenance":{"origin":"admission"}}'),
  ('detent', 'lane', 'entered', '{"provenance":{"origin":"routine"}}'),
  ('detent', 'lane', 'entered', '{}');
`); err != nil {
		t.Fatalf("seed backlog_admission_runs error = %v", err)
	}

	diagnostic, err := readDoctorAdmissionDiagnostic(ctx, db, "detent", doctorAdmissionDiagnostic{
		Schedule:          "0 6 * * 1-5",
		MaximumGapSeconds: int64(doctorAdmissionMaximumGap("0 6 * * 1-5") / time.Second),
		CriteriaSection:   "Admission criteria",
		Dimensions:        []string{"Alignment", "Risk"},
		NeverRun:          true,
	})
	if err != nil {
		t.Fatalf("readDoctorAdmissionDiagnostic() error = %v", err)
	}
	check := doctorAdmissionCheck("admission", diagnostic, "")
	if check.Status != doctorWarn {
		t.Fatalf("Status = %q, want %q", check.Status, doctorWarn)
	}
	for _, want := range []string{
		"3 consecutive failures",
		`schedule "0 6 * * 1-5"`,
		"maximum cadence gap 72h0m0s",
		"observed cost 20s/run across 3 recent runs, 30s and 38000 tokens/candidate-bearing run across 2 admission agent sessions",
		"3 of 3 recent runs found 12 eligible candidates after filters (30 source candidate observations)",
		"candidates are accumulating between runs",
		`"Admission criteria"`,
		"2 project-defined dimensions",
		"candidates=12 evaluated=4 proposed=1",
		"skipped=excluded_label:2",
		"truncated=candidate_cap:8",
		"issues=digitaldrywood/detent#1535",
		"origins=admission:1,routine:1,unknown:1",
		"proposal_outcomes=accepted:1,expired:1",
		"average_decision_seconds=accepted:60,expired:604800",
		"accepted_downstream=completed:1,rework:2,review_churn:3,spend:$4.50",
	} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("Detail = %q, want %q", check.Detail, want)
		}
	}
	if diagnostic.NeverRun || diagnostic.CandidatesFound != 12 || diagnostic.CandidatesEvaluated != 4 || diagnostic.Proposed != 1 ||
		diagnostic.ObservedRuns != 3 || diagnostic.CandidateBearingRuns != 3 ||
		diagnostic.CandidatesObserved != 30 || diagnostic.EligibleCandidates != 12 ||
		diagnostic.AverageRunSeconds != 20 ||
		diagnostic.AdmissionSessions != 2 || diagnostic.AverageSessionSeconds != 30 ||
		diagnostic.AverageSessionTokens != 38000 {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
	if diagnostic.Skipped["excluded_label"] != 2 || diagnostic.Truncated["candidate_cap"] != 8 {
		t.Fatalf("diagnostic counts = skipped %#v truncated %#v", diagnostic.Skipped, diagnostic.Truncated)
	}
	if diagnostic.Origins["admission"] != 1 || diagnostic.Origins["unknown"] != 1 ||
		diagnostic.ProposalOutcomes["expired"] != 1 ||
		diagnostic.DecisionSeconds["accepted"] != 60 ||
		diagnostic.AcceptedCompleted != 1 ||
		diagnostic.ReworkCount != 2 ||
		diagnostic.ReviewChurnCount != 3 ||
		diagnostic.SpendUSD != 4.5 {
		t.Fatalf("evidence diagnostic = %#v", diagnostic)
	}
}

func TestDoctorAdmissionCadenceGuidance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		schedule           string
		observed           int
		withEligible       int
		sourceCandidates   int
		eligibleCandidates int
		wantStatus         doctorStatus
		wantText           string
	}{
		{
			name:               "responsive cadence with candidates",
			schedule:           "*/15 * * * *",
			observed:           4,
			withEligible:       2,
			sourceCandidates:   3,
			eligibleCandidates: 2,
			wantStatus:         doctorOK,
		},
		{
			name:       "slow cadence without candidates",
			schedule:   "0 6 * * 1-5",
			observed:   4,
			wantStatus: doctorOK,
		},
		{
			name:             "slow cadence with only filtered candidates",
			schedule:         "0 6 * * 1-5",
			observed:         4,
			sourceCandidates: 7,
			wantStatus:       doctorOK,
		},
		{
			name:               "slow cadence with accumulating candidates",
			schedule:           "0 6 * * 1-5",
			observed:           4,
			withEligible:       2,
			sourceCandidates:   7,
			eligibleCandidates: 5,
			wantStatus:         doctorWarn,
			wantText:           "candidates are accumulating between runs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			diagnostic := doctorAdmissionDiagnostic{
				Schedule:              tt.schedule,
				MaximumGapSeconds:     int64(doctorAdmissionMaximumGap(tt.schedule) / time.Second),
				CriteriaSection:       "Admission criteria",
				Dimensions:            []string{"Alignment"},
				ObservedRuns:          tt.observed,
				CandidateBearingRuns:  tt.withEligible,
				CandidatesObserved:    tt.sourceCandidates,
				EligibleCandidates:    tt.eligibleCandidates,
				AverageRunSeconds:     25,
				AdmissionSessions:     3,
				AverageSessionSeconds: 24,
				AverageSessionTokens:  36000,
			}
			check := doctorAdmissionCheck("admission", diagnostic, "")
			if check.Status != tt.wantStatus {
				t.Fatalf("Status = %q, want %q: %s", check.Status, tt.wantStatus, check.Detail)
			}
			if tt.wantText != "" && !strings.Contains(check.Detail, tt.wantText) {
				t.Fatalf("Detail = %q, want %q", check.Detail, tt.wantText)
			}
			if tt.wantText == "" && strings.Contains(check.Detail, "cadence guidance") {
				t.Fatalf("Detail = %q, want no cadence guidance", check.Detail)
			}
			for _, want := range []string{
				"schedule " + strconvQuote(tt.schedule),
				"observed cost 25s/run across",
				"36000 tokens/candidate-bearing run",
			} {
				if !strings.Contains(check.Detail, want) {
					t.Fatalf("Detail = %q, want %q", check.Detail, want)
				}
			}
		})
	}
}

func TestDoctorAdmissionMaximumGap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		schedule string
		want     time.Duration
	}{
		{name: "quarter hourly", schedule: "*/15 * * * *", want: 15 * time.Minute},
		{name: "hourly", schedule: "0 * * * *", want: time.Hour},
		{name: "weekdays", schedule: "0 6 * * 1-5", want: 72 * time.Hour},
		{name: "configured timezone", schedule: "CRON_TZ=America/New_York 0 6 * * 1-5", want: 73 * time.Hour},
		{name: "invalid", schedule: "not cron", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := doctorAdmissionMaximumGap(tt.schedule); got != tt.want {
				t.Fatalf("doctorAdmissionMaximumGap(%q) = %s, want %s", tt.schedule, got, tt.want)
			}
		})
	}
}

func TestDoctorAdmissionWarnsForStatesOnlyPublicExposure(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerGitHub
	cfg.Tracker.Repository = "digitaldrywood/detent"
	cfg.BacklogAdmission.Enabled = true
	cfg.BacklogAdmission.CriteriaSection = "Admission criteria"
	cfg.BacklogAdmission.Sources.States = []string{"Backlog"}
	check := checkDoctorAdmission(context.Background(), "detent", workflowconfig.Workflow{
		Config: cfg,
		SharedPrompt: `## Admission criteria

- **Risk** — requires a bounded recovery path.
`,
	}, "", doctorDeps{
		githubRepositoryInfo: func(context.Context, workflowconfig.Config, string) (ghconnector.RepositoryInfo, error) {
			return ghconnector.RepositoryInfo{Visibility: "public"}, nil
		},
	})
	if check.BacklogAdmission == nil ||
		check.BacklogAdmission.RepositoryVisibility != "public" ||
		!strings.Contains(strings.Join(check.BacklogAdmission.Warnings, "; "), "untrusted issue authors") {
		t.Fatalf("check = %#v", check)
	}
}

func TestDoctorAdmissionWarnsWhenNeverRunOrCriteriaUnresolvable(t *testing.T) {
	cfg := workflowconfig.Default()
	cfg.BacklogAdmission.Enabled = true
	cfg.BacklogAdmission.CriteriaSection = "Admission criteria"

	unresolved := checkDoctorAdmission(context.Background(), "detent", workflowconfig.Workflow{
		Config:       cfg,
		SharedPrompt: "# Workflow\n",
	}, "", doctorDeps{})
	if unresolved.Status != doctorWarn || !strings.Contains(unresolved.Detail, "was not found") {
		t.Fatalf("unresolved check = %#v", unresolved)
	}

	neverRun := checkDoctorAdmission(context.Background(), "detent", workflowconfig.Workflow{
		Config: cfg,
		SharedPrompt: `# Workflow

## Admission criteria

- **Risk** — requires a bounded recovery path.
`,
	}, "", doctorDeps{})
	if neverRun.Status != doctorWarn || !strings.Contains(neverRun.Detail, "runtime store is not configured") {
		t.Fatalf("never-run check = %#v", neverRun)
	}
	if neverRun.BacklogAdmission == nil || len(neverRun.BacklogAdmission.Dimensions) != 1 {
		t.Fatalf("BacklogAdmission = %#v", neverRun.BacklogAdmission)
	}
}
