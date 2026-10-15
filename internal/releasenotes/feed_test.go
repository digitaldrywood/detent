package releasenotes

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDerive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subject, body, category, text string
		pr                            int
	}{
		{"feat(hub): add account feed (#101)", "", "New", "Add account feed", 101},
		{"perf!: reduce latency (#102)", "", "Improved", "Reduce latency", 102},
		{"fix(runner): retain stale pull refs (#103) (#202)", "", "Fixed", "Retain stale pull refs", 103},
		{"revert: remove obsolete mode (#104)", "", "Removed", "Remove obsolete mode", 104},
		{"fix: complete prj_example#12 (#105)", "\n\nfix(ci): scheduled Browser Visual (1/2) failure\n\nDetails", "Fixed", "Scheduled Browser Visual (1/2) failure", 105},
		{"fix: complete prj_example#12 (#105)", "Retain stale pull refs", "Fixed", "Retain stale pull refs", 105},
		{"fix: complete prj_example#12 (#105)", "", "Fixed", "Maintenance update", 105},
		{"fix: complete prj_example#12 (#105)", " \n\t", "Fixed", "Maintenance update", 105},
		{"fix: complete prj_example#12 (#105)", "fix: complete prj_example#12", "Fixed", "Maintenance update", 105},
		{"fix: complete prj_example#12 (#105) (#205)", "fix: complete prj_example#12 (#105)", "Fixed", "Maintenance update", 105},
		{"fix: complete prj_example#12 (#105)", "fix(hub):", "Fixed", "Maintenance update", 105},
		{"fix: complete prj_example#12 (#105)", "feat(hub): add release feed", "New", "Add release feed", 105},
		{"fix: éliminate duplicates (#106)", "", "Fixed", "Éliminate duplicates", 106},
		{"fix: unmerged work", "", "", "", 0},
	}
	for _, kind := range []string{"test", "ci", "chore", "build", "docs", "release", "style"} {
		tests = append(tests, struct {
			subject, body, category, text string
			pr                            int
		}{kind + ": internal work (#107)", "", "", "", 107})
	}
	for _, test := range tests {
		t.Run(test.subject+test.body, func(t *testing.T) {
			got, public := Derive(Commit{Subject: test.subject, Body: test.body})
			if public != (test.category != "") || got.Category != test.category || got.Text != test.text || got.PRNumber != test.pr {
				t.Fatalf("entry=%+v public=%t", got, public)
			}
			if public && got.PRURL != fmt.Sprintf("https://github.com/digitaldrywood/detent/pull/%d", test.pr) {
				t.Fatalf("link=%q", got.PRURL)
			}
		})
	}
}

func TestAppendDeduplicatesBuilds(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	first := Append(Build{Version: "1.2.0"}, []Commit{{SHA: "a", Subject: "feat: add feed (#101)"}, {SHA: "b", Subject: "test: cover feed (#102)"}, {SHA: "c", Subject: "docs: update guide"}}, seen)
	next := Append(Build{Version: "1.2.0-op.r.2.g123456789abc"}, []Commit{{SHA: "pick", Subject: "feat: add feed (#101) (#201)"}, {SHA: "b", Subject: "test: cover feed (#102)"}, {SHA: "c", Subject: "docs: update guide"}, {SHA: "d", Subject: "fix: retain state (#103)"}}, seen)
	if first.Version != "v1.2.0" || first.Patch || len(first.Entries) != 1 || first.InternalCount != 2 {
		t.Fatalf("first=%+v", first)
	}
	if !next.Patch || len(next.Entries) != 1 || next.Entries[0].PRNumber != 103 || next.InternalCount != 0 {
		t.Fatalf("next=%+v", next)
	}
}

func TestRead(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, raw          string
		missing, wantError bool
	}{
		{name: "unprepared", missing: true}, {name: "empty", raw: "[]"}, {name: "invalid", raw: `[{"entries":"private text"}]`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := fstest.MapFS{}
			if !test.missing {
				files["release-notes.json"] = &fstest.MapFile{Data: []byte(test.raw)}
			}
			builds, err := Read(files)
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private text") {
				t.Fatal("error contains entry text")
			}
			if err == nil {
				raw, e := json.Marshal(builds)
				if e != nil || string(raw) != "[]" {
					t.Fatalf("builds=%s error=%v", raw, e)
				}
			}
		})
	}
}
