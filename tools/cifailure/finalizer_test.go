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
		name, repository         string
		requirements, auditState string
		awaitsStatus             bool
		failed, unavailable      bool
		unchanged                bool
		cancelledRun             bool
		cancelledJob             bool
		timedOutJob              bool
		workflowCancelled        bool
		develop                  bool
		staleHead                bool
		newSeries                bool
		differentSeries          bool
		releaseRun               bool
		newerFailure             bool
		preflightSkipped         bool
	}{
		{name: "missing release dispatch recovered without recreating tag", repository: "digitaldrywood/detent", unchanged: true},
		{name: "existing release workflow is not redispatched", repository: "digitaldrywood/detent", unchanged: true, releaseRun: true},
		{name: "newer release failure blocks existing tag dispatch", repository: "digitaldrywood/detent", unchanged: true, newerFailure: true},
		{name: "required status missing", repository: "digitaldrywood/detent", requirements: `["release-security-audit"]`, awaitsStatus: true},
		{name: "required status failure", repository: "digitaldrywood/detent", requirements: `["release-security-audit"]`, auditState: "failure", awaitsStatus: true},
		{name: "required status success", repository: "digitaldrywood/detent", requirements: `["scheduled-full-ci", "release-security-audit"]`, auditState: "success"},
		{name: "empty requirements", repository: "digitaldrywood/detent", requirements: `[]`},
		{name: "native green evidence", repository: "digitaldrywood/detent"},
		{name: "develop records evidence without tagging", repository: "digitaldrywood/detent", develop: true},
		{name: "preflight skip reports activity without validation publication", repository: "digitaldrywood/detent", develop: true, preflightSkipped: true},
		{name: "skipped reporting failure retains evidence without release", repository: "digitaldrywood/detent", develop: true, preflightSkipped: true, unavailable: true},
		{name: "moved release head does not tag", repository: "digitaldrywood/detent", staleHead: true},
		{name: "new release series starts at patch zero", repository: "digitaldrywood/detent", newSeries: true},
		{name: "older series tag does not suppress new series", repository: "digitaldrywood/detent", newSeries: true, unchanged: true, differentSeries: true},
		{name: "native failure evidence", repository: "digitaldrywood/detent", failed: true},
		{name: "native publication unavailable", repository: "digitaldrywood/detent", unavailable: true},
		{name: "GitHub green closure", repository: "owner/other"},
		{name: "cancelled run with failed jobs", repository: "digitaldrywood/detent", failed: true, cancelledRun: true},
		{name: "operator cancellation before any cancelled job is visible", repository: "digitaldrywood/detent", workflowCancelled: true},
		{name: "operator-cancelled workflow with failed jobs", repository: "digitaldrywood/detent", failed: true, cancelledJob: true, workflowCancelled: true},
		{name: "cancelled job without workflow cancellation reports failure", repository: "digitaldrywood/detent", cancelledJob: true},
		{name: "timed-out job reports failure", repository: "digitaldrywood/detent", timedOutJob: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var jobs []job
			for id := range 15 {
				jobs = append(jobs, job{ID: int64(id + 1), Name: "Validation", Conclusion: "success"})
			}
			if tt.preflightSkipped {
				jobs[0].Name = "preflight"
				for i := 1; i < len(jobs); i++ {
					jobs[i].Conclusion = "skipped"
				}
			}
			if tt.failed {
				jobs[0].Conclusion = "failure"
			}
			if tt.cancelledJob {
				jobs[1].Conclusion = "cancelled"
			}
			if tt.timedOutJob {
				jobs[1].Conclusion = "timed_out"
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
  *'/runs/123/artifacts?per_page=100'*) printf '7\n' ;;
  *'-X DELETE '*'/actions/artifacts/7') ;;
  *'/statuses/'*) printf '123\n' ;;
  *'/commits/'*'/status')
    if [ -n "$FIXTURE_AUDIT_STATE" ]; then
      printf '{"sha":"%s","statuses":[{"context":"release-security-audit","state":"%s"}]}\n' "$CI_DEVELOP_SHA" "$FIXTURE_AUDIT_STATE"
    elif [ "$FIXTURE_NEWER_FAILURE" = true ]; then
      printf '{"sha":"%s","statuses":[{"context":"scheduled-full-ci","id":124,"state":"failure"}]}\n' "$CI_DEVELOP_SHA"
    else
      printf '{"sha":"%s","statuses":[{"context":"scheduled-full-ci","id":123,"state":"success"}]}\n' "$CI_DEVELOP_SHA"
    fi ;;
  *'/workflows/release.yml/runs?'*)
    if [ "$FIXTURE_RELEASE_RUN" = true ]; then
      printf '[{"workflow_runs":[{"head_branch":"v1.2.3"}]}]\n'
    else
      printf '[{"workflow_runs":[{"head_branch":"v1.1.9"}]}]\n'
    fi ;;
  'issue list '*) printf '[{"number":42}]\n' ;;
  *'/dispatches'*|'issue close '*) ;;
  *) exit 9 ;;
esac
`,
				"git": `#!/usr/bin/env bash
set -euo pipefail
printf 'repository %s\n' "$*" >> "$FIXTURE_LOG"
case "$*" in
  'ls-remote '*) printf '%s\trefs/heads/release/v1.2\n' "$FIXTURE_HEAD" ;;
  'tag --list '*) if [ "$FIXTURE_NEW_SERIES" = false ]; then printf 'v1.2.3\n'; fi ;;
  'tag --points-at '*) if [ "$FIXTURE_UNCHANGED" = true ] && [[ "$*" == *'v1.2.*'* ]]; then printf 'v1.2.3\n'; fi ;;
  'cat-file -t '*) printf 'tag\n' ;;
  'for-each-ref '*) printf '<!-- detent-release-provenance:{"schema":1,"repository":"%s","tag":"v1.2.3","commit":"%s","checks":[{"name":"scheduled-full-ci","status":"completed","conclusion":"success","status_id":123}]} -->\n' "$GITHUB_REPOSITORY" "$CI_DEVELOP_SHA" ;;
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
			command.Env = append(command.Env, "CI_SHOULD_RUN="+map[bool]string{true: "false", false: "true"}[tt.preflightSkipped])
			releaseBranch := "release/v1.2"
			if tt.differentSeries {
				releaseBranch = "release/v1.3"
			}
			if tt.develop {
				releaseBranch = ""
			}
			head := scheduledEnv("CI_DEVELOP_SHA")
			if tt.staleHead {
				head = strings.Repeat("b", 40)
			}
			command.Env = append(command.Env, "CI_RELEASE_BRANCH="+releaseBranch, "FIXTURE_HEAD="+head, "FIXTURE_NEW_SERIES="+map[bool]string{true: "true", false: "false"}[tt.newSeries])
			command.Env = append(command.Env, "FIXTURE_RELEASE_RUN="+map[bool]string{true: "true", false: "false"}[tt.releaseRun], "FIXTURE_NEWER_FAILURE="+map[bool]string{true: "true", false: "false"}[tt.newerFailure])
			command.Env = append(command.Env, "DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON="+tt.requirements, "FIXTURE_AUDIT_STATE="+tt.auditState)
			skipped := tt.cancelledRun || tt.workflowCancelled
			output, err := command.CombinedOutput()
			if (err != nil) != ((tt.failed || tt.unavailable || tt.cancelledJob || tt.timedOutJob || tt.newerFailure) && !skipped) {
				t.Fatalf("finalizer = %v: %s", err, output)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(calls)
			deleted := strings.Contains(text, "-X DELETE repos/"+tt.repository+"/actions/artifacts/7")
			wantDeleted := !tt.develop && (skipped || tt.failed || tt.cancelledJob || tt.timedOutJob || tt.staleHead || tt.newerFailure)
			if deleted != wantDeleted {
				t.Fatalf("prebuilt artifacts deleted = %v, want %v: %s", deleted, wantDeleted, text)
			}
			if skipped {
				if !strings.Contains(string(output), "cancelled; skipping reporting and release") || strings.Contains(text, "native ") || strings.Contains(text, "repository ") || strings.Contains(text, "/statuses/") || strings.Contains(text, "/dispatches") || strings.Contains(text, "forge issue") {
					t.Fatalf("cancelled run produced reporting or release effects: %s: %s", text, output)
				}
				if tt.cancelledRun && strings.Contains(text, "/jobs?") {
					t.Fatal("cancelled run fetched job failures")
				}
				return
			}
			if tt.awaitsStatus {
				if !strings.Contains(string(output), "awaits required status release-security-audit") || !strings.Contains(text, "context=scheduled-full-ci") || strings.Contains(text, "tag -a ") || strings.Contains(text, "/dispatches") || deleted {
					t.Fatalf("waiting for audit produced incorrect effects: %s: %s", text, output)
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
			if tt.preflightSkipped {
				if strings.Count(text, "native run ./tools/cifailure") != 1 || strings.Contains(text, "repository ") || strings.Contains(text, "/statuses/") || strings.Contains(text, "/dispatches") || strings.Contains(text, "forge issue") {
					t.Fatalf("skipped execution published validation or release evidence: %s", text)
				}
				if _, err := os.Stat(filepath.Join(dir, "scheduled-ci-publisher-jobs.json")); !os.IsNotExist(err) {
					t.Fatal("skipped report failure became a publisher failure")
				}
				return
			}
			if tt.unchanged && !tt.differentSeries {
				if strings.Contains(text, "tag -a ") || strings.Contains(text, "/statuses/") {
					t.Fatalf("existing release evidence was replaced: %s", text)
				}
				wantDispatch := !tt.releaseRun && !tt.newerFailure
				if strings.Contains(text, "/dispatches") != wantDispatch {
					t.Fatalf("release dispatch recovery = %v, want %v: %s", !wantDispatch, wantDispatch, text)
				}
			} else if tt.develop || tt.staleHead {
				if strings.Contains(text, "tag -a ") || strings.Contains(text, "/dispatches") {
					t.Fatal("non-release validation published a release")
				}
				if tt.develop && (!strings.Contains(text, "context=develop-full-ci") || strings.Contains(text, "context=scheduled-full-ci")) {
					t.Fatalf("develop overwrote release evidence: %s", text)
				}
			} else if tt.failed || tt.cancelledJob || tt.timedOutJob {
				if strings.Contains(text, "repository tag -a") || strings.Contains(text, "/statuses/") {
					t.Fatal("failed suite published green evidence")
				}
			} else {
				tag := "v1.2.4"
				if tt.newSeries {
					tag = "v1.2.0"
				}
				if tt.differentSeries {
					tag = "v1.3.0"
				}
				if !strings.Contains(text, "repository tag -a "+tag+" "+scheduledEnv("CI_DEVELOP_SHA")) || !strings.Contains(text, "refs/tags/"+tag) || !strings.Contains(text, "/release.yml/dispatches") {
					t.Fatalf("scheduled tag evidence lost: %s", text)
				}
			}
			if tt.unavailable {
				retained, err := os.ReadFile(filepath.Join(dir, "scheduled-ci-jobs.json"))
				if err != nil || strings.Contains(string(retained), "Finalize scheduled validation") {
					t.Fatal("publisher failure replaced suite result")
				}
				if _, err := os.Stat(filepath.Join(dir, "scheduled-ci-publisher-jobs.json")); !os.IsNotExist(err) {
					t.Fatal("reporting failure after release publication reran the reporter")
				}
				if strings.Count(text, "native run ./tools/cifailure") != 1 || !strings.Contains(string(output), "Validated release v1.2.4 is published; reporting the green suite to the tracker failed") {
					t.Fatalf("reporting failure after release publication: %s: %s", text, output)
				}
			}
		})
	}
}
