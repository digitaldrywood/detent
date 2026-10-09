package runnerauth

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMergeProblems(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	old := NewProblem("backend_missing")
	old.FirstSeen = now.Add(-time.Hour)
	oldProject := old
	oldProject.ProjectID = "one"
	newProject := NewProblem("backend_missing")
	newProject.ProjectID = "two"
	project := NewProblem("policy_mismatch")
	project.ProjectID = "alpha"
	project.FirstSeen = now.Add(-2 * time.Hour)
	second := NewProblem("policy_mismatch")
	second.ProjectID = "beta"
	for _, test := range []struct {
		name              string
		previous, current []Problem
		codes             []string
	}{
		{"new", nil, []Problem{NewProblem("tier_unavailable")}, []string{"tier_unavailable"}},
		{"continuing", []Problem{old}, []Problem{NewProblem("backend_missing")}, []string{"backend_missing"}},
		{"cleared", []Problem{old}, nil, []string{}},
		{"project failures remain distinct", []Problem{oldProject}, []Problem{newProject, oldProject}, []string{"backend_missing/one", "backend_missing/two"}},
		{"recovered project clears independently", []Problem{oldProject}, []Problem{newProject}, []string{"backend_missing/two"}},
		{"deduplicated and sorted", []Problem{old}, []Problem{NewProblem("tier_unavailable"), NewProblem("backend_missing"), NewProblem("backend_missing")}, []string{"backend_missing", "tier_unavailable"}},
		{"project problems retain separate timestamps", []Problem{project}, []Problem{second, project, second}, []string{"policy_mismatch/alpha", "policy_mismatch/beta"}},
		{"one project clears independently", []Problem{project, second}, []Problem{second}, []string{"policy_mismatch/beta"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := MergeProblems(test.previous, test.current, now)
			codes := []string{}
			for _, p := range got {
				identity := p.Code
				if p.ProjectID != "" {
					identity += "/" + p.ProjectID
				}
				codes = append(codes, identity)
				want := now
				for _, previous := range test.previous {
					if p.Code == previous.Code && p.ProjectID == previous.ProjectID && !previous.FirstSeen.IsZero() {
						want = previous.FirstSeen
					}
				}
				if !p.FirstSeen.Equal(want) || p.Message == "" || p.FixHint == "" {
					t.Fatalf("problem = %+v", p)
				}
			}
			if !reflect.DeepEqual(codes, test.codes) {
				t.Fatalf("codes=%v want=%v", codes, test.codes)
			}
		})
	}
}

func TestValidateReportedProblems(t *testing.T) {
	for _, code := range []string{"tier_unavailable", "backend_missing", "host_service_unreachable", "settings_invalid", "keep_awake_failed", "home_project_unservable", "settings_rejected", "version_unsupported", "unknown"} {
		t.Run(code, func(t *testing.T) {
			want := code == "tier_unavailable" || code == "backend_missing" || code == "host_service_unreachable" || code == "settings_invalid" || code == "keep_awake_failed"
			if got := ValidateReportedProblems([]Problem{NewProblem(code)}) == nil; got != want {
				t.Fatalf("valid=%v want=%v", got, want)
			}
		})
	}
	p := NewProblem("backend_missing")
	otherProject := p
	otherProject.ProjectID = "site"
	if err := ValidateReportedProblems([]Problem{p, otherProject}); err != nil {
		t.Fatalf("distinct project problems refused: %v", err)
	}
	for _, problems := range [][]Problem{{p, p}, {{Code: p.Code}}, {{Code: p.Code, Message: strings.Repeat("x", 1001), FixHint: p.FixHint}}} {
		if ValidateReportedProblems(problems) == nil {
			t.Fatalf("invalid problems accepted: %+v", problems)
		}
	}
}

func TestProblemDetails(t *testing.T) {
	for _, test := range []struct{ name, input, forbidden string }{
		{"API key", "OPENAI_API_KEY=sk-private-key failed", "sk-private-key"},
		{"JSON", `{"access_token":"private-token", "error":"not signed in"}`, "private-token"},
		{"header", "Authorization: Bearer private-token\nnot signed in", "private-token"},
		{"argument", "codex --api-key private-key login status", "private-key"},
		{"URL", "https://user:private-password@example.test failed", "private-password"},
		{"bounded", strings.Repeat("error ", 600), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := SanitizeProblem(Problem{ErrorOutput: test.input, Check: test.input})
			if len(p.ErrorOutput) > 2000 || len(p.Check) > 2000 {
				t.Fatal("output was not bounded")
			}
			if test.forbidden != "" && strings.Contains(p.ErrorOutput+p.Check, test.forbidden) {
				t.Fatal("credential was reported")
			}
		})
	}
	now := time.Now()
	a := NewProblem("backend_missing")
	a.Subject = "codex"
	a.Check = "codex login status"
	a.ReportedAt = now.Add(-time.Minute)
	b := a
	b.Subject = "claude"
	problems := MergeProblems(nil, []Problem{a, b}, now)
	if len(problems) != 2 || !problems[0].ReportedAt.Equal(a.ReportedAt) {
		t.Fatalf("distinct reports lost: %+v", problems)
	}
	if err := ValidateReportedProblems(problems); err != nil {
		t.Fatal(err)
	}
	read := MergeProblems(problems, problems, now.Add(time.Hour))
	if !read[0].ReportedAt.Equal(a.ReportedAt) {
		t.Fatal("read refreshed report time")
	}
}
