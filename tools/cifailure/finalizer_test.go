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
	}{
		{"native unchanged validated commit", "digitaldrywood/detent", false, false, true},
		{"native green evidence", "digitaldrywood/detent", false, false, false},
		{"native failure evidence", "digitaldrywood/detent", true, false, false},
		{"native publication unavailable", "digitaldrywood/detent", false, true, false},
		{"GitHub green closure", "owner/other", false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var jobs []job
			for id := range 16 {
				jobs = append(jobs, job{ID: int64(id + 1), Name: "Validation", Conclusion: "success"})
			}
			if tt.failed {
				jobs[0].Conclusion = "failure"
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
			output, err := command.CombinedOutput()
			if (err != nil) != (tt.failed || tt.unavailable) {
				t.Fatalf("finalizer = %v: %s", err, output)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(calls)
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
			} else if tt.failed {
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
