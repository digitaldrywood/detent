package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScheduledFinalizer(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required for the scheduled shell fixture")
	}
	for _, tt := range []struct {
		name, repository    string
		failed, unavailable bool
		unchanged           bool
		cancelledRun        bool
		cancelledJob        bool
		workflowCancelled   bool
	}{
		{name: "native unchanged validated commit", repository: "digitaldrywood/detent", unchanged: true},
		{name: "native green evidence", repository: "digitaldrywood/detent"},
		{name: "native failure evidence", repository: "digitaldrywood/detent", failed: true},
		{name: "native publication unavailable", repository: "digitaldrywood/detent", unavailable: true},
		{name: "GitHub green closure", repository: "owner/other"},
		{name: "cancelled run with failed jobs", repository: "digitaldrywood/detent", failed: true, cancelledRun: true},
		{name: "operator-cancelled workflow with failed jobs", repository: "digitaldrywood/detent", failed: true, cancelledJob: true, workflowCancelled: true},
		{name: "timed-out job reports failure", repository: "digitaldrywood/detent", cancelledJob: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var jobs []job
			for id := range 15 {
				jobs = append(jobs, job{ID: int64(id + 1), Name: "Validation", Conclusion: "success"})
			}
			if tt.failed {
				jobs[0].Conclusion = "failure"
			}
			if tt.cancelledJob {
				jobs[1].Conclusion = "cancelled"
			}
			var fixture strings.Builder
			for _, j := range jobs {
				raw, err := json.Marshal(j)
				if err != nil {
					t.Fatal(err)
				}
				fixture.Write(raw)
				fixture.WriteByte('\n')
			}
			if err := os.WriteFile(filepath.Join(dir, "jobs"), []byte(fixture.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			programs := map[string]string{
				"gh": `#!/usr/bin/env bash
set -euo pipefail
printf 'forge %s\n' "$*" >> "$FIXTURE_LOG"
case "$*" in
  *'/attempts/1 --jq .conclusion') printf '%s\n' "$FIXTURE_CONCLUSION" ;;
  *'/jobs?per_page=100'*) cat "$FIXTURE_JOBS" ;;
  *'/statuses/'*) printf '123\n' ;;
  'issue list '*) printf '[{"number":42}]\n' ;;
  *'/dispatches'*|'issue close '*) ;;
  *) exit 9 ;;
esac
`,
				"git": `#!/usr/bin/env bash
set -euo pipefail
printf 'repository %s\n' "$*" >> "$FIXTURE_LOG"
case "$*" in
  'tag --list '*) printf 'v1.2.3\n' ;;
  'tag --points-at '*) if [ "$FIXTURE_UNCHANGED" = true ]; then printf 'v1.2.3\n'; fi ;;
  'cat-file -t '*) printf 'tag\n' ;;
  'for-each-ref '*) printf '%s\n' '{"name":"scheduled-full-ci"}' ;;
esac
`,
				"go": `#!/usr/bin/env bash
set -euo pipefail
printf 'native %s\n' "$*" >> "$FIXTURE_LOG"
cat >> "$FIXTURE_REPORTS"
printf '{"retained":"publication payload"}\n'
test "$FIXTURE_UNAVAILABLE" = false
`,
			}
			for name, program := range programs {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(program), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			root, err := filepath.Abs(filepath.Join("..", ".."))
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "bash", "scripts/scheduled-ci-finish.sh")
			command.Dir = root
			command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+dir, "GITHUB_REPOSITORY="+tt.repository, "GITHUB_SERVER_URL=https://github.com", "GITHUB_RUN_ID=123", "GITHUB_RUN_ATTEMPT=1", "CI_DEVELOP_SHA="+scheduledEnv("CI_DEVELOP_SHA"), "FIXTURE_LOG="+filepath.Join(dir, "calls"), "FIXTURE_JOBS="+filepath.Join(dir, "jobs"), "FIXTURE_REPORTS="+filepath.Join(dir, "reports"), "FIXTURE_UNCHANGED="+map[bool]string{true: "true", false: "false"}[tt.unchanged], "FIXTURE_UNAVAILABLE="+map[bool]string{true: "true", false: "false"}[tt.unavailable])
			command.Env = append(command.Env, "FIXTURE_CONCLUSION="+map[bool]string{true: "cancelled", false: "null"}[tt.cancelledRun], "WORKFLOW_CANCELLED="+map[bool]string{true: "true", false: "false"}[tt.workflowCancelled])
			skipped := tt.cancelledRun || tt.workflowCancelled
			output, err := command.CombinedOutput()
			if (err != nil) != ((tt.failed || tt.unavailable || tt.cancelledJob) && !skipped) {
				t.Fatalf("finalizer = %v: %s", err, output)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(calls)
			if skipped {
				if !strings.Contains(string(output), "cancelled; skipping reporting and release") || strings.Contains(text, "native ") || strings.Contains(text, "repository ") || strings.Contains(text, "/statuses/") || strings.Contains(text, "/dispatches") || strings.Contains(text, "forge issue") {
					t.Fatalf("cancelled run produced reporting or release effects: %s: %s", text, output)
				}
				if tt.cancelledRun && strings.Contains(text, "/jobs?") {
					t.Fatal("cancelled run fetched job failures")
				}
				return
			}
			if tt.repository == "digitaldrywood/detent" {
				if strings.Contains(text, "forge issue") || !strings.Contains(text, "native run ./tools/cifailure") {
					t.Fatalf("wrong destination: %s", text)
				}
				payload, err := os.ReadFile(filepath.Join(dir, "scheduled-ci-evidence.jsonl"))
				if err != nil || !strings.Contains(string(payload), "publication payload") {
					t.Fatal("evidence not retained")
				}
			} else if !strings.Contains(text, "forge issue close 42") || strings.Contains(text, "native run") {
				t.Fatalf("GitHub behavior changed: %s", text)
			}
			if tt.unchanged {
				if strings.Contains(text, "tag -a ") || strings.Contains(text, "/dispatches") {
					t.Fatal("unchanged validated commit released again")
				}
			} else if tt.failed || tt.cancelledJob {
				if strings.Contains(text, "repository tag -a") || strings.Contains(text, "/statuses/") {
					t.Fatal("failed suite published green evidence")
				}
			} else {
				if !strings.Contains(text, "repository tag -a v1.2.4 "+scheduledEnv("CI_DEVELOP_SHA")) || !strings.Contains(text, "refs/tags/v1.2.4") || !strings.Contains(text, "/release.yml/dispatches") {
					t.Fatalf("scheduled tag evidence lost: %s", text)
				}
			}
			if tt.unavailable {
				retained, err := os.ReadFile(filepath.Join(dir, "scheduled-ci-jobs.json"))
				if err != nil || strings.Contains(string(retained), "Finalize scheduled validation") {
					t.Fatal("publisher failure replaced suite result")
				}
				problem, err := os.ReadFile(filepath.Join(dir, "scheduled-ci-publisher-jobs.json"))
				if err != nil || !strings.Contains(string(problem), "Finalize scheduled validation") {
					t.Fatal("publisher evidence missing")
				}
			}
		})
	}
}
