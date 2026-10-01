package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/issueorigin"
)

func TestParseProblems(t *testing.T) {
	t.Parallel()
	const dispatch = "go-test:github.com/digitaldrywood/detent/internal/orchestrator:TestDispatchReadyIssuesLogsDebugDecisionAndWorkerLifecycle"
	for _, tt := range []struct {
		name, log string
		want      []string
	}{
		{"coverage", fixture(t, "coverage"), []string{dispatch}},
		{"race JSON", fixture(t, "race"), []string{dispatch}},
		{"distinct tests", "--- FAIL: TestOne (0s)\n one_test.go:1: broken\n--- FAIL: TestTwo (0s)\n two_test.go:1: broken\nFAIL\towner/repo/pkg\t0.1s", []string{"go-test:owner/repo/pkg:TestOne", "go-test:owner/repo/pkg:TestTwo"}},
		{"same name in distinct packages", "--- FAIL: TestOne (0s)\nFAIL\towner/repo/one\t0.1s\n--- FAIL: TestOne (0s)\nFAIL\towner/repo/two\t0.1s", []string{"go-test:owner/repo/one:TestOne", "go-test:owner/repo/two:TestOne"}},
		{"subtest parents do not duplicate repairs", "--- FAIL: TestOne (0s)\n    --- FAIL: TestOne/a (0s)\n    --- FAIL: TestOne/b (0s)\nFAIL\towner/repo/pkg\t0.1s", []string{"go-test:owner/repo/pkg:TestOne/a", "go-test:owner/repo/pkg:TestOne/b"}},
		{"parent with independent assertion", "--- FAIL: TestOne (0s)\n    one_test.go:12: parent assertion failed\n    --- FAIL: TestOne/a (0s)\nFAIL\towner/repo/pkg\t0.1s", []string{"go-test:owner/repo/pkg:TestOne", "go-test:owner/repo/pkg:TestOne/a"}},
		{"JSON subtest parents", `{"Action":"fail","Package":"owner/repo/pkg","Test":"TestOne/a"}` + "\n" + `{"Action":"fail","Package":"owner/repo/pkg","Test":"TestOne"}`, []string{"go-test:owner/repo/pkg:TestOne/a"}},
		{"source tools and duplicate diagnostics", "\x1b[31minternal/one.go:12:3: Potential nil panic detected\x1b[0m\ninternal/one.go:12:3: Potential nil panic detected\ninternal/two.go:20:7: unused value (staticcheck)", []string{"go-diagnostic:internal/one.go:12:3:Potential nil panic detected", "go-diagnostic:internal/two.go:20:7:unused value (staticcheck)"}},
		{"workspace path normalization", "/runner/work/repo/repo/internal/one.go:12:3: error\n./internal/one.go:12:3: error", []string{"go-diagnostic:internal/one.go:12:3:error"}},
		{"tool JSON output", `{"Action":"output","Package":"owner/repo/pkg","Output":"internal/one.go:12:3: error\n"}`, []string{"go-diagnostic:internal/one.go:12:3:error"}},
		{"different diagnostic messages", "internal/one.go:12:3: first error\ninternal/one.go:12:3: second error", []string{"go-diagnostic:internal/one.go:12:3:first error", "go-diagnostic:internal/one.go:12:3:second error"}},
		{"truncated test without package", "--- FAIL: TestOne (0s)\nfile_test.go:12: missing value", nil},
		{"generic exit and setup errors", "##[error]Process completed with exit code 1.\nnetwork download failed", nil},
		{"nonrepository diagnostics", "D:/runner/cache/main.go:12:3: error\n./../cache/main.go:12:3: error", nil},
		{"unrelated absolute diagnostic", "/runner/cache/tool/main.go:12:3: error", nil},
		{"successful output", "ok\towner/repo/pkg\t0.1s", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var keys []string
			for _, p := range parseProblems(tt.log, "/runner/work/repo/repo") {
				keys = append(keys, p.Key)
			}
			if !reflect.DeepEqual(keys, tt.want) {
				t.Fatalf("problems = %v; want %v", keys, tt.want)
			}
		})
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".log")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type fakeGH struct {
	issues     []openIssue
	logs       map[int64]string
	comments   map[int][]string
	created    []openIssue
	listErr    error
	logErr     error
	publishErr error
	listOutput string
}

func (f *fakeGH) command(_ context.Context, input string, args ...string) ([]byte, error) {
	if len(args) > 1 && args[1] == "graphql" {
		if !strings.Contains(strings.Join(args, " "), "--paginate") || strings.Contains(strings.Join(args, " "), "labels:") {
			return nil, errors.New("open issue lookup must paginate repository-wide")
		}
		if f.listErr != nil || f.listOutput != "" {
			return []byte(f.listOutput), f.listErr
		}
		var out strings.Builder
		for _, issue := range f.issues {
			encoded, err := json.Marshal(issue)
			if err != nil {
				return nil, err
			}
			out.Write(encoded)
			out.WriteByte('\n')
		}
		return []byte(out.String()), nil
	}
	if len(args) >= 2 && args[0] == "api" && strings.HasSuffix(args[len(args)-1], "/logs") {
		var id int64
		if _, err := fmt.Sscanf(args[len(args)-1], "repos/digitaldrywood/detent/actions/jobs/%d/logs", &id); err != nil {
			return nil, err
		}
		if strings.Contains(f.logs[id], "\x1b") && !slices.Contains(args, "--allow-escape-sequences") {
			return nil, errors.New("the response contains terminal escape sequences; pass --allow-escape-sequences to output it anyway")
		}
		return []byte(f.logs[id]), f.logErr
	}
	if len(args) < 4 || args[2] != "POST" {
		return nil, fmt.Errorf("unexpected gh command: %v", args)
	}
	if f.publishErr != nil {
		return nil, f.publishErr
	}
	var payload struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		return nil, err
	}
	if strings.HasSuffix(args[3], "/comments") {
		var number int
		if _, err := fmt.Sscanf(args[3], "repos/digitaldrywood/detent/issues/%d/comments", &number); err != nil {
			return nil, err
		}
		if f.comments == nil {
			f.comments = map[int][]string{}
		}
		f.comments[number] = append(f.comments[number], payload.Body)
		return []byte(`{}`), nil
	}
	if !reflect.DeepEqual(payload.Labels, []string{"detent:todo", "hotfix", "ci-scheduled-failure"}) || payload.Title == "" || len([]rune(payload.Title)) > 256 {
		return nil, errors.New("missing scheduled repair metadata")
	}
	created := openIssue{Number: 100 + len(f.created), Body: payload.Body}
	f.created = append(f.created, created)
	f.issues = append(f.issues, created)
	return json.Marshal(created)
}

func scheduledEnv(key string) string {
	return map[string]string{
		"GITHUB_REPOSITORY":  "digitaldrywood/detent",
		"GITHUB_SERVER_URL":  "https://github.com",
		"GITHUB_RUN_ID":      "36804639457",
		"GITHUB_RUN_ATTEMPT": "1",
		"CI_DEVELOP_SHA":     "cac5c45031aa9ec5038f18282553197a469e7fc2",
	}[key]
}

func TestReport(t *testing.T) {
	t.Parallel()
	const jobs = `[{"id":1,"name":"Test Coverage","conclusion":"failure","html_url":"coverage-job"},{"id":2,"name":"Verify race (1)","conclusion":"failure","html_url":"race-job"},{"id":3,"name":"Lint","conclusion":"success"},{"id":4,"name":"Finalize scheduled validation","conclusion":null}]`
	for _, tt := range []struct {
		name         string
		logs         map[int64]string
		preexisting  bool
		logErr       error
		wantIssues   int
		wantComments int
	}{
		{"coverage and race consolidate", map[int64]string{1: fixture(t, "coverage"), 2: fixture(t, "race")}, false, nil, 1, 3},
		{"ANSI coverage and race consolidate", map[int64]string{1: "\x1b[31m" + fixture(t, "coverage") + "\x1b[0m", 2: "\x1b[31m" + fixture(t, "race") + "\x1b[0m"}, false, nil, 1, 3},
		{"repository-wide existing machine problem", map[int64]string{1: fixture(t, "coverage"), 2: fixture(t, "race")}, true, nil, 0, 4},
		{"distinct tests stay separate", map[int64]string{1: "--- FAIL: TestOne (0s)\n--- FAIL: TestTwo (0s)\nFAIL\towner/repo/pkg\t0s", 2: "--- FAIL: TestTwo (0s)\nFAIL\towner/repo/pkg\t0s"}, false, nil, 2, 4},
		{"distinct tools consolidate across jobs", map[int64]string{1: "internal/one.go:12:3: first diagnostic\ninternal/two.go:20:7: second diagnostic", 2: "internal/one.go:12:3: first diagnostic"}, false, nil, 2, 4},
		{"unknown evidence keeps job identity", map[int64]string{1: "exit code 1", 2: "exit code 1"}, false, nil, 2, 2},
		{"unreadable logs keep job identity", nil, false, errors.New("logs unavailable"), 2, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gh := &fakeGH{logs: tt.logs, logErr: tt.logErr}
			// A plain operator mention of a fingerprint must never count as an origin.
			key := "go-test:github.com/digitaldrywood/detent/internal/orchestrator:TestDispatchReadyIssuesLogsDebugDecisionAndWorkerLifecycle"
			fp := issueorigin.Fingerprint(key)
			gh.issues = []openIssue{{Number: 9, Body: "fingerprint: " + fp}}
			if tt.preexisting {
				gh.issues = append(gh.issues, openIssue{Number: 42, Body: issueorigin.Stamp("Worker discovery, without a scheduled CI label", issueorigin.Origin{Kind: "worker", Fingerprint: fp})})
			}
			for run := range 2 {
				getenv := func(key string) string {
					if key == "GITHUB_RUN_ID" {
						return strconv.Itoa(36804639457 + run)
					}
					return scheduledEnv(key)
				}
				if err := report(t.Context(), strings.NewReader(jobs), gh.command, getenv); err != nil {
					t.Fatal(err)
				}
			}
			var comments []string
			for number, bodies := range gh.comments {
				if number == 9 {
					t.Fatal("operator text was mistaken for machine origin")
				}
				comments = append(comments, bodies...)
			}
			if len(gh.created) != tt.wantIssues || len(comments) != tt.wantComments {
				t.Fatalf("created %d issues and %d comments; want %d and %d", len(gh.created), len(comments), tt.wantIssues, tt.wantComments)
			}
			for _, comment := range comments {
				if !strings.HasPrefix(comment, "## New machine occurrence\n") {
					t.Fatalf("not an occurrence: %s", comment)
				}
			}
			for _, issue := range gh.created {
				origin, ok := issueorigin.Parse(issue.Body)
				if !ok || origin.Kind != "doctor" || origin.Instance != "github-actions" || !strings.Contains(issue.Body, scheduledEnv("CI_DEVELOP_SHA")) || !strings.Contains(origin.Source, "/attempts/1") || !strings.Contains(issue.Body, "effort: high") {
					t.Fatalf("invalid machine issue: %+v", issue)
				}
			}
			if strings.Contains(tt.name, "job identity") {
				for i, j := range []string{"Test Coverage", "Verify race (1)"} {
					origin, _ := issueorigin.Parse(gh.created[i].Body)
					if origin.Fingerprint != legacyJobFingerprint("scheduled-ci:digitaldrywood/detent:"+j) {
						t.Fatal("unknown evidence changed the conservative legacy fingerprint")
					}
				}
			} else {
				all := strings.Join(comments, "\n")
				for _, issue := range gh.created {
					all += issue.Body
				}
				if !strings.Contains(all, "race-job") || !strings.Contains(all, "coverage-job") {
					t.Fatal("affected job evidence was lost")
				}
				if strings.HasSuffix(tt.name, "coverage and race consolidate") {
					if strings.Count(all, "skip_reason=already_running") != 4 || strings.Contains(all, "Job logs could not be read") {
						t.Fatal("test diagnostics were lost from an occurrence")
					}
				}
			}
		})
	}
}

func TestReportFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, input string
		backend     fakeGH
		wantErr     bool
		wantIssues  int
	}{
		{"malformed jobs", `{`, fakeGH{}, true, 0},
		{"lookup failure never files blindly", `[{"id":1,"name":"Lint","conclusion":"failure"}]`, fakeGH{listErr: errors.New("unavailable")}, true, 0},
		{"malformed issue response", `[]`, fakeGH{listOutput: `{`}, true, 0},
		{"filing failure", `[{"id":1,"name":"Lint","conclusion":"failure"}]`, fakeGH{publishErr: errors.New("unavailable")}, true, 0},
		{"long diagnostic title remains fileable", `[{"id":1,"name":"Lint","conclusion":"failure"}]`, fakeGH{logs: map[int64]string{1: "internal/one.go:12:3: " + strings.Repeat("diagnostic ", 100)}}, false, 1},
		{"finalizer failure retains reporting", `[{"id":0,"name":"Finalize scheduled validation","conclusion":"failure"}]`, fakeGH{}, false, 1},
		{"all green", `[{"id":1,"name":"Lint","conclusion":"success"}]`, fakeGH{}, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := report(t.Context(), strings.NewReader(tt.input), tt.backend.command, scheduledEnv)
			if (err != nil) != tt.wantErr || len(tt.backend.created) != tt.wantIssues {
				t.Fatalf("report = %v, %d issues; want error %v, %d issues", err, len(tt.backend.created), tt.wantErr, tt.wantIssues)
			}
		})
	}
}
