package store

import (
	"math"
	"testing"
	"time"
)

func TestOperationsRecordedHistory(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		populated bool
	}{{"empty", false}, {"recorded", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backend := openTestStore(t, t.Context()).(*sqliteStore)
			if tc.populated {
				statements := []string{
					`INSERT INTO efficiency_receipts(project_id,issue_id,pr_number,attempts,total_tokens,first_dispatched_at,completed_at) VALUES ('p','1',1,1,100,'2026-09-10T00:00:00Z','2026-09-11T10:00:00Z'),('p','2',NULL,2,300,'2026-09-10T00:00:00Z','2026-09-11T11:00:00Z'),('p','future',3,1,900,'2026-09-10T00:00:00Z','2026-09-11T12:00:00Z')`,
					`INSERT INTO workflow_phase_events(project_id,issue_id,phase_type,phase_name,status,started_at,event_day) VALUES ('p','1','lane','In Progress','entered','2026-09-01T10:00:00Z','2026-09-01'),('p','1','lane','In Progress','entered','2026-09-10T10:00:00Z','2026-09-10'),('p','1','lane','Done','entered','2026-09-11T10:00:00Z','2026-09-11'),('p','1','lane','Done','entered','2026-09-11T11:00:00Z','2026-09-11'),('p','2','lane','In Progress','entered','2026-09-11T09:00:00Z','2026-09-11'),('p','2','lane','Done','entered','2026-09-11T11:00:00Z','2026-09-11'),('p','1','lane','Blocked','entered','2026-09-11T08:00:00Z','2026-09-11'),('p','1','lane','Blocked','entered','2026-09-11T09:00:00Z','2026-09-11'),('q','1','lane','Blocked','entered','2026-09-11T09:00:00Z','2026-09-11')`,
					`INSERT INTO lane_ledger(project_id,issue_id,from_state,to_state,reason,written_at,result,origin,action_kind,resolved_at) VALUES ('p','1','Blocked','Todo','blocked_cause_recovery','2026-09-11T10:00:00Z','applied','operator_routine','return_retired_parks','2026-09-11T10:00:01Z'),('p','2','Blocked','Todo','blocked_cause_recovery','2026-09-11T10:00:00Z','failed','operator_routine','return_retired_parks','2026-09-11T10:00:01Z'),('p','3','Blocked','Todo','blocked_cause_recovery','2026-09-11T10:00:00Z','applied','','','2026-09-11T10:00:01Z')`,
					`INSERT INTO human_questions(project_id,issue_id,question_key,issue_identifier,body,question_comment_id,answer_comment_id) VALUES ('p','1','choice','owner/repo#1','Choose a delivery target?','123',''),('p','2','answered','owner/repo#2','Old question','124','125'),('p','3','unpublished','owner/repo#3','Not sent','','')`,
					`INSERT INTO scheduler_decisions(project_id,issue_id,identifier,lane,result,reason,selected,decision_at) VALUES ('p','1','owner/repo#1','Todo','selected','ready',1,'2026-09-11T10:00:00Z'),('p','2','owner/repo#2','Todo','skipped','capacity',0,'2026-09-11T10:00:00Z')`,
				}
				for _, stmt := range statements {
					if _, err := backend.db.ExecContext(t.Context(), stmt); err != nil {
						t.Fatal(err)
					}
				}
			}
			report, err := backend.OperationsReport(t.Context(), now, now.Add(-time.Hour*24))
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Stats) != 2 {
				t.Fatalf("windows: %#v", report)
			}
			if !tc.populated {
				if report.Stats[0].CycleMedianSeconds != nil || len(report.Actions) != 0 || len(report.Decisions) != 0 {
					t.Fatalf("empty: %#v", report)
				}
				return
			}
			w := report.Stats[0]
			if w.Closes != 2 || w.Merges != 1 || *w.TokensPerCompletedIssue != 200 || *w.CleanAttemptRate != .5 {
				t.Fatalf("stats: %#v", w)
			}
			if w.CycleSamples != 2 || math.Abs(*w.CycleMedianSeconds-435600) > 0.001 || math.Abs(*w.CycleP75Seconds-649800) > 0.001 {
				t.Fatalf("cycle: %#v median %v p75 %v", w, *w.CycleMedianSeconds, *w.CycleP75Seconds)
			}
			if len(w.BlockedNights) != 1 || w.BlockedNights[0].Issues != 2 {
				t.Fatalf("blocked: %#v", w.BlockedNights)
			}
			if len(w.Projects) != 1 || w.Projects[0].Dispatches != 1 || w.Projects[0].SkipReasons[0].Reason != "capacity" {
				t.Fatalf("projects: %#v", w.Projects)
			}
			if len(report.Actions) != 1 || report.Actions[0].Kind != "return_retired_parks" {
				t.Fatalf("actions: %#v", report.Actions)
			}
			if len(report.Decisions) != 1 || report.Decisions[0].URL != "https://github.com/owner/repo/issues/1#issuecomment-123" {
				t.Fatalf("decisions: %#v", report.Decisions)
			}
			refreshed, err := backend.OperationsReport(t.Context(), now, now)
			if err != nil || len(refreshed.Actions) != 0 {
				t.Fatalf("refresh: %#v %v", refreshed, err)
			}
		})
	}
}

func TestOperationsReportUsesResolutionTimeForActions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t, t.Context()).(*sqliteStore)
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO lane_ledger(project_id,issue_id,from_state,to_state,reason,written_at,result,origin,action_kind,resolved_at) VALUES ('p','late','Blocked','Todo','blocked_cause_recovery','2026-09-11T09:00:00Z','applied','operator_routine','return_retired_parks','2026-09-11T11:30:00Z')`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	report, err := s.OperationsReport(t.Context(), now, time.Date(2026, 9, 11, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Actions) != 1 || report.Actions[0].Issue != "late" || report.Actions[0].Kind != "return_retired_parks" || report.Actions[0].Reason != "blocked_cause_recovery" {
		t.Fatalf("actions = %#v, want the write resolved after the cursor even though it was prepared before it", report.Actions)
	}
}
