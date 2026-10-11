package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// BenchmarkSQLiteRefresh replays history reads for ten projects and 300 open
// issues. For incident profiling, DETENT_SQLITE_PROFILE_DB may name an isolated
// online backup; never point it at a running instance's database.
func BenchmarkSQLiteRefresh(b *testing.B) {
	path := os.Getenv("DETENT_SQLITE_PROFILE_DB")
	if path == "" {
		path = filepath.Join(b.TempDir(), "refresh.db")
	}
	s, err := openSQLite(b.Context(), Config{Path: path})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	if os.Getenv("DETENT_SQLITE_PROFILE_DB") == "" {
		// Bulk seed outside the measured interval, retaining wide historical rows
		// so the benchmark catches sorts that spill payloads to temporary files.
		_, err = s.db.ExecContext(b.Context(), `WITH RECURSIVE n(id) AS (VALUES(1) UNION ALL SELECT id+1 FROM n WHERE id<180000)
INSERT INTO scheduler_decisions(project_id,issue_id,identifier,issue_url,result,decision_at,metadata_json)
SELECT printf('project-%d',(id-1)%10),printf('issue-%d',(id-1)%300),printf('owner/repo#%d',(id-1)%300),printf('https://example.com/%d',(id-1)%300),'skipped',printf('2026-09-29T00:%02d:%02dZ',(id/60)%60,id%60),json_object('payload',printf('%02000d',id)) FROM n;
WITH RECURSIVE n(id) AS (VALUES(1) UNION ALL SELECT id+1 FROM n WHERE id<18000)
INSERT INTO codex_sessions(project_id,issue_id,identifier,issue_url,started_at,completed_at,final_state,provider_session_id,total_tokens)
SELECT printf('project-%d',(id-1)%10),printf('issue-%d',(id-1)%300),printf('owner/repo#%d',(id-1)%300),printf('https://example.com/%d',(id-1)%300),'2026-09-29T00:00:00Z','2026-09-29T00:01:00Z','completed','provider',100 FROM n;`)
		if err != nil {
			b.Fatal(err)
		}
	}
	rows, err := s.db.QueryContext(b.Context(), `SELECT project_id,issue_id,COALESCE(identifier,''),COALESCE(issue_url,'') FROM codex_sessions WHERE project_id IS NOT NULL AND issue_id IS NOT NULL GROUP BY project_id,issue_id ORDER BY project_id,issue_id`)
	if err != nil {
		b.Fatal(err)
	}
	defer rows.Close()
	byProject := map[string][]IssueIdentity{}
	var projects []string
	for rows.Next() {
		var issue IssueIdentity
		if err := rows.Scan(&issue.ProjectID, &issue.IssueID, &issue.Identifier, &issue.IssueURL); err != nil {
			b.Fatal(err)
		}
		if len(byProject[issue.ProjectID]) == 0 {
			projects = append(projects, issue.ProjectID)
		}
		byProject[issue.ProjectID] = append(byProject[issue.ProjectID], issue)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		b.Fatal(err)
	}
	var issues []IssueIdentity
	if len(projects) < 10 {
		b.Fatalf("fixture has %d projects; need ten", len(projects))
	}
	projects = projects[:10]
	for _, project := range projects {
		for i := range 30 {
			issue := IssueIdentity{ProjectID: project, IssueID: fmt.Sprintf("unseen-%d", i)}
			if i < len(byProject[project]) {
				issue = byProject[project][i]
			}
			issues = append(issues, issue)
		}
	}
	before := sqliteProfileIO()
	var cycles int64
	b.ReportAllocs()
	for b.Loop() {
		for _, project := range projects {
			if _, err := s.ListRecentSchedulerDecisions(b.Context(), SchedulerDecisionQuery{ProjectID: project, Limit: 500}); err != nil {
				b.Fatal(err)
			}
		}
		for _, issue := range issues {
			if _, err := s.IssueTokenSpend(b.Context(), issue); err != nil {
				b.Fatal(err)
			}
			if _, err := s.LatestIssueAgentResumeState(b.Context(), issue); err != nil && !errors.Is(err, ErrNotFound) {
				b.Fatal(err)
			}
			if _, err := s.ListIssueAIDebugWorkAttempts(b.Context(), issue); err != nil {
				b.Fatal(err)
			}
			if _, err := s.IssueCardHistory(b.Context(), issue, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)); err != nil {
				b.Fatal(err)
			}
		}
		cycles++
	}
	for name, after := range sqliteProfileIO() {
		b.ReportMetric(float64(after-before[name])/float64(cycles), name+"/refresh")
	}
}

func sqliteProfileIO() map[string]int64 {
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		return nil // Linux profiling counters; the benchmark also runs elsewhere.
	}
	out := map[string]int64{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || (key != "rchar" && key != "wchar" && key != "write_bytes") {
			continue
		}
		out[key], _ = strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	}
	return out
}
