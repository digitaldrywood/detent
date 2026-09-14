package cli

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDoctorInvariantAdmission(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	human := `{"provenance":{"origin":"human","initiator":"human","actor":{"login":"operator","kind":"User"}}}`
	for _, tt := range []struct {
		name, title, body, origin, reason, metadata string
		age                                         time.Duration
		result, project, target                     string
		missing                                     bool
		want                                        doctorStatus
	}{
		{name: "feature admitted", title: "feat(ui): add view", reason: "admission_proposal_accepted", want: doctorFail},
		{name: "human feature", title: "feat: add view", origin: "human", want: doctorOK},
		{name: "evidenced removal fix", title: "fix: remove lease", body: "Attempt 5601 logged duplicate ownership. Remove the existing lease.", reason: "admission_proposal_accepted", want: doctorOK},
		{name: "operator human", title: "perf: speed up dispatch", origin: "operator", reason: "operator_move", metadata: human, want: doctorOK},
		{name: "current operator ledger blank origin", title: "refactor(core)!: new dispatch", reason: "kanban_move", metadata: human, want: doctorOK},
		{name: "authenticated dashboard human without tracker actor", title: "feat: add view", reason: "kanban_move", metadata: `{"provenance":{"schema":2,"origin":"human","initiator":"human","basis":"authenticated_human_session"}}`, want: doctorOK},
		{name: "dashboard agent session", title: "feat: add view", reason: "kanban_move", metadata: `{"provenance":{"schema":2,"origin":"agent","initiator":"detent_agent_session","basis":"active_agent_session"}}`, want: doctorFail},
		{name: "operator without actor", title: "feat: add view", origin: "operator", reason: "operator_move", metadata: `{"provenance":{"origin":"human","initiator":"human"}}`, want: doctorFail},
		{name: "assistant using human login", title: "feat: add view", origin: "agent", reason: "operator_move", metadata: human, want: doctorFail},
		{name: "routine using human login", title: "feat: add view", origin: "operator_routine", reason: "operator_move", metadata: human, want: doctorFail},
		{name: "admission actor is not scope approval", title: "feat: add view", origin: "human", reason: "admission_proposal_accepted", metadata: human, want: doctorFail},
		{name: "fix expands mechanism", title: "fix: recover dispatch", body: "Add a new recovery path for lost workers.", reason: "admission_proposal_accepted", want: doctorFail},
		{name: "unknown origin", title: "feat: add view", want: doctorFail},
		{name: "old entry", title: "feat: add view", age: 8 * 24 * time.Hour, want: doctorOK},
		{name: "window boundary", title: "feat: add view", age: 7 * 24 * time.Hour, want: doctorFail},
		{name: "future entry", title: "feat: add view", age: -time.Hour, want: doctorOK},
		{name: "failed write", title: "feat: add view", result: "failed", want: doctorOK},
		{name: "prepared write", title: "feat: add view", result: "prepared", want: doctorOK},
		{name: "other project", title: "feat: add view", project: "beta", want: doctorOK},
		{name: "other lane", title: "feat: add view", target: "Backlog", want: doctorOK},
		{name: "missing issue", missing: true, want: doctorWarn},
		{name: "later human event cannot approve earlier move", title: "feat: add view", reason: "operator_move", metadata: human, age: 2 * time.Hour, want: doctorFail},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			age := tt.age
			if age == 0 {
				age = time.Hour
			}
			at := now.Add(-age).Format(time.RFC3339Nano)
			result := tt.result
			if result == "" {
				result = "applied"
			}
			project := tt.project
			if project == "" {
				project = "alpha"
			}
			target := tt.target
			if target == "" {
				target = "Todo"
			}
			path := doctorInvariantFixtureDB(t)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			_, err = db.ExecContext(t.Context(), `INSERT INTO lane_ledger(project_id,issue_id,from_state,to_state,reason,written_at,resolved_at,result,origin) VALUES (?, 'issue-1','Backlog',?,?,?,?,?,?)`, project, target, tt.reason, at, at, result, tt.origin)
			if err != nil {
				t.Fatal(err)
			}
			// Event timestamp is deliberately independent of the ledger timestamp.
			_, err = db.ExecContext(t.Context(), `INSERT INTO workflow_phase_events(project_id,issue_id,identifier,phase_type,phase_name,previous_phase_name,reason,started_at,metadata_json) VALUES (?,'issue-1','owner/repo#1','lane',?,'Backlog',?,?,?)`, project, target, tt.reason, now.Add(-time.Hour).Format(time.RFC3339Nano), tt.metadata)
			if err != nil {
				t.Fatal(err)
			}
			cfg := workflowconfig.Config{}
			cfg.Tracker.Kind = "memory"
			if !tt.missing {
				cfg.Tracker.Issues = []connector.Issue{{ID: "issue-1", Identifier: "owner/repo#1", Title: tt.title, Description: tt.body}}
			}
			checks := checkDoctorInvariantEvidence(t.Context(), "alpha", globalconfig.Project{}, cfg, path, doctorDeps{now: func() time.Time { return now }})
			got, detail := doctorInvariantStatus(t, checks, "INV-11")
			if got != tt.want {
				t.Fatalf("status=%s detail=%s; want %s", got, detail, tt.want)
			}
			if got == doctorFail {
				for _, piece := range []string{"owner/repo#1", fmt.Sprintf("origin=%q", tt.origin), at} {
					if !strings.Contains(detail, piece) {
						t.Errorf("detail %q missing %q", detail, piece)
					}
				}
			}
		})
	}
}

func TestDoctorInvariantScopeClassification(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		title, body string
		want        bool
	}{
		{"feat(api)!: extend", "", true}, {"PERF: improve", "", true}, {"refactor: simplify", "", true},
		{"fix: repair", "Introduce a config key `worker.wait`.", true},
		{"fix: repair", "Adds a reason code for waiting.", true},
		{"fix: repair", "Expand the existing breaker to cover startup.", true},
		{"fix: repair", "New reservation for merge work.", true},
		{"fix: repair", "Add a brake, lease, or park.", true},
		{"fix: repair", "Remove the old lease. No new config key or reason code.", false},
		{"fix: repair", "Do not add a new recovery path.", false},
		{"fix: repair", "Consolidate existing breakers. Introduce a new lease.", true},
		{"fix: repair", "Attempt 123 logs show the lease leaked. Remove it.", false},
		{"feature flags broken", "Repair incorrect parsing.", false},
	} {
		t.Run(tt.title+"/"+tt.body, func(t *testing.T) {
			if got := doctorIssueRequiresScopeApproval(tt.title, tt.body); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestDoctorInvariantAdmissionRepeatedEntries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	path := doctorInvariantFixtureDB(t,
		`INSERT INTO lane_ledger(project_id,issue_id,from_state,to_state,reason,written_at,result,origin) VALUES
  ('alpha','issue-1','Backlog','Todo','admission_proposal_accepted','2026-09-14T10:00:00Z','applied','admission'),
  ('alpha','issue-1','Backlog','Todo','operator_move','2026-09-14T11:00:00Z','applied','human'),
  ('alpha','issue-2','Backlog','Todo','operator_move','2026-09-14T11:30:00Z','applied','agent')`)
	cfg := workflowconfig.Config{}
	cfg.Tracker.Kind = "memory"
	cfg.Tracker.Issues = []connector.Issue{
		{ID: "issue-1", Identifier: "owner/repo#1", Title: "feat: add view"},
		{ID: "issue-2", Identifier: "owner/repo#2", Title: "fix: recover", Description: "Introduce a new lease."},
	}
	checks := checkDoctorInvariantEvidence(t.Context(), "alpha", globalconfig.Project{}, cfg, path, doctorDeps{now: func() time.Time { return now }})
	status, detail := doctorInvariantStatus(t, checks, "INV-11")
	if status != doctorFail {
		t.Fatalf("%s: %s", status, detail)
	}
	for _, want := range []string{"3 applied Todo entries", `owner/repo#1 origin="admission"`, `owner/repo#2 origin="agent"`, "2026-09-14T10:00:00Z", "2026-09-14T11:30:00Z"} {
		if !strings.Contains(detail, want) {
			t.Errorf("missing %q in %s", want, detail)
		}
	}
	if strings.Contains(detail, "2026-09-14T11:00:00Z") {
		t.Errorf("human move reported: %s", detail)
	}
}

func TestDoctorInvariantAdmissionUnavailableEvidence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, kind         string
		noStore, oldSchema bool
	}{
		{name: "unavailable store", noStore: true},
		{name: "old schema", oldSchema: true},
		{name: "unavailable tracker", kind: "unsupported"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := doctorInvariantFixtureDB(t, `INSERT INTO lane_ledger(project_id,issue_id,to_state,written_at,result,origin) VALUES ('alpha','issue-1','Todo','2026-09-14T10:00:00Z','applied','agent')`)
			if tt.noStore {
				path = path + ".missing"
			}
			if tt.oldSchema {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.ExecContext(t.Context(), `ALTER TABLE lane_ledger DROP COLUMN origin`)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			cfg := workflowconfig.Config{}
			cfg.Tracker.Kind = tt.kind
			checks := checkDoctorInvariantEvidence(t.Context(), "alpha", globalconfig.Project{}, cfg, path, doctorDeps{now: func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }})
			status, detail := doctorInvariantStatus(t, checks, "INV-11")
			if status != doctorWarn || !strings.Contains(detail, "scope evidence unavailable") {
				t.Fatalf("%s: %s", status, detail)
			}
		})
	}
}
