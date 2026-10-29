package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/releaseprovenance"
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
		name, repository                                                                   string
		requirements, auditState                                                           string
		awaitsStatus                                                                       bool
		rerun, auditThenRerun, missingJob, skippedJob, latestFailed, repaired, wrongJobSHA bool
		missingAudit, wrongSHA, superseded, oldProvenance, rejectReuse, unset              bool
		failed, unavailable                                                                bool
		unchanged                                                                          bool
		cancelledRun                                                                       bool
		cancelledJob                                                                       bool
		timedOutJob                                                                        bool
		workflowCancelled                                                                  bool
		develop                                                                            bool
		staleHead                                                                          bool
		newSeries                                                                          bool
		differentSeries                                                                    bool
		releaseRun                                                                         bool
		newerFailure                                                                       bool
		pageBoundary                                                                       bool
		preflightSkipped                                                                   bool
	}{
		{name: "missing release dispatch recovered without recreating tag", repository: "digitaldrywood/detent", unchanged: true},
		{name: "existing release workflow is not redispatched", repository: "digitaldrywood/detent", unchanged: true, releaseRun: true},
		{name: "newer release failure blocks existing tag dispatch", repository: "digitaldrywood/detent", unchanged: true, newerFailure: true},
		{name: "required status missing", repository: "digitaldrywood/detent", requirements: `["release-security-audit"]`, missingAudit: true, awaitsStatus: true},
		{name: "required status failure", repository: "digitaldrywood/detent", requirements: `["release-security-audit"]`, auditState: "failure", awaitsStatus: true},
		{name: "required status success", repository: "digitaldrywood/detent", requirements: `["scheduled-full-ci", "release-security-audit"]`, auditState: "success"},
		{name: "empty requirements", repository: "digitaldrywood/detent", requirements: `[]`, missingAudit: true, awaitsStatus: true},
		{name: "unset requirements still require audit", repository: "digitaldrywood/detent", unset: true, missingAudit: true, awaitsStatus: true},
		{name: "empty variable still requires audit", repository: "digitaldrywood/detent", missingAudit: true, awaitsStatus: true},
		{name: "configured extras recorded", repository: "digitaldrywood/detent", requirements: `["extra"]`},
		{name: "audit on another SHA", repository: "digitaldrywood/detent", wrongSHA: true, awaitsStatus: true},
		{name: "audit superseded by failure", repository: "digitaldrywood/detent", superseded: true, awaitsStatus: true},
		{name: "reuse missing audit", repository: "digitaldrywood/detent", unchanged: true, missingAudit: true, rejectReuse: true},
		{name: "reuse old provenance", repository: "digitaldrywood/detent", unchanged: true, oldProvenance: true, rejectReuse: true},
		{name: "reuse failed audit", repository: "digitaldrywood/detent", unchanged: true, auditState: "failure", rejectReuse: true},
		{name: "reuse superseded audit", repository: "digitaldrywood/detent", unchanged: true, superseded: true, rejectReuse: true},
		{name: "reuse another SHA", repository: "digitaldrywood/detent", unchanged: true, wrongSHA: true, rejectReuse: true},
		{name: "failed suite then finalize only after audit", repository: "digitaldrywood/detent", failed: true, rerun: true},
		{name: "green suite awaits audit then finalize tags", repository: "digitaldrywood/detent", missingAudit: true, awaitsStatus: true, auditThenRerun: true},
		{name: "finalize only missing required job", repository: "digitaldrywood/detent", missingJob: true, rerun: true},
		{name: "suite jobs on another SHA", repository: "digitaldrywood/detent", wrongJobSHA: true},
		{name: "skipped required job", repository: "digitaldrywood/detent", skippedJob: true},
		{name: "latest attempt failure overrides success", repository: "digitaldrywood/detent", latestFailed: true, rerun: true},
		{name: "latest attempt success replaces failure", repository: "digitaldrywood/detent", repaired: true, rerun: true},
		{name: "older success on page one newer failure on page two", repository: "digitaldrywood/detent", latestFailed: true, rerun: true, pageBoundary: true},
		{name: "older failure on page one newer success on page two", repository: "digitaldrywood/detent", repaired: true, rerun: true, pageBoundary: true},
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
			names := []string{"preflight", "Invariant Gate", "Lint", "Verify build and vet", "Generated sources", "Cloud client", "Security", "GoReleaser Snapshot", "Installer Smoke (ubuntu-latest)"}
			for i := 1; i <= 5; i++ {
				names = append(names, fmt.Sprintf("Verify race (%d/5)", i))
			}
			for i := 1; i <= 5; i++ {
				names = append(names, fmt.Sprintf("Verify race (hub %d/5)", i), fmt.Sprintf("Test Coverage (hub %d/5)", i))
			}
			for i := 1; i <= 4; i++ {
				names = append(names, fmt.Sprintf("Test Coverage (%d/4)", i))
			}
			for i := 1; i <= 3; i++ {
				names = append(names, fmt.Sprintf("Browser Visual (%d/3)", i))
			}
			for _, platform := range []string{"darwin", "linux", "windows"} {
				for _, arch := range []string{"amd64", "arm64"} {
					names = append(names, "Release prebuild ("+platform+"/"+arch+")")
				}
			}
			var jobs []job
			for id, name := range names {
				jobs = append(jobs, job{ID: int64(id + 1), Name: name, Conclusion: "success"})
			}
			if tt.missingJob {
				jobs = jobs[1:]
			}
			if tt.skippedJob {
				jobs[0].Conclusion = "skipped"
			}
			if tt.repaired {
				jobs[0].Conclusion = "failure"
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
			if tt.pageBoundary {
				for len(jobs) < 50 {
					jobs = append(jobs, jobs[1])
				}
			}
			var fixture strings.Builder
			for _, j := range jobs {
				raw, err := json.Marshal(j)
				if err != nil {
					t.Fatal(err)
				}
				var record map[string]any
				if err := json.Unmarshal(raw, &record); err != nil {
					t.Fatal(err)
				}
				record["run_attempt"] = 1
				record["head_sha"] = scheduledEnv("CI_DEVELOP_SHA")
				if tt.wrongJobSHA {
					record["head_sha"] = strings.Repeat("b", 40)
				}
				raw, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				fixture.Write(raw)
				fixture.WriteByte('\n')
			}
			if tt.latestFailed || tt.repaired {
				conclusion := "failure"
				if tt.repaired {
					conclusion = "success"
				}
				fmt.Fprintf(&fixture, `{"id":100,"name":"preflight","run_attempt":2,"head_sha":%q,"conclusion":%q}`+"\n", scheduledEnv("CI_DEVELOP_SHA"), conclusion)
			}
			if tt.rerun {
				fixture.WriteString(`{"id":200,"name":"Finalize scheduled validation","run_attempt":2,"conclusion":null}` + "\n")
			}
			if err := os.WriteFile(filepath.Join(dir, "jobs"), []byte(fixture.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			programs := map[string]string{
				"gh": `#!/usr/bin/env bash
set -euo pipefail
printf 'forge %s\n' "$*" >> "$FIXTURE_LOG"
case "$*" in
  *'/attempts/'*' --jq .conclusion') printf '%s\n' "$FIXTURE_CONCLUSION" ;;
  *'/jobs?filter=all&per_page='*)
    page_size=0
    paginate=false
    for arg in "$@"; do
      case "$arg" in
        *'/jobs?filter=all&per_page='*) page_size="${arg##*per_page=}" ;;
        --paginate) paginate=true ;;
      esac
    done
    if [ "$page_size" -ge 100 ]; then
      echo "HTTP 502" >&2
      exit 1
    fi
    test "$page_size" -gt 0
    count="$(jq -s length "$FIXTURE_JOBS")"
    for ((offset=0; offset<count; offset+=page_size)); do
      jq -s --argjson offset "$offset" --argjson size "$page_size" '.[$offset:$offset + $size][]' "$FIXTURE_JOBS"
      if [ "$paginate" = false ]; then break; fi
    done ;;
  *'/jobs?per_page=50'*) if [ "$GITHUB_RUN_ATTEMPT" = 2 ]; then printf '%s\n' '{"id":200,"name":"Finalize scheduled validation","conclusion":null}'; exit; fi; cat "$FIXTURE_JOBS" ;;
  *'/runs/123/artifacts?per_page=100'*) printf '7\n' ;;
  *'-X DELETE '*'/actions/artifacts/7') ;;
  *'/statuses/'*) printf '123\n' ;;
  *'/commits/'*'/status?per_page=100') cat "$FIXTURE_STATUSES" ;;
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
  'for-each-ref '*) cat "$FIXTURE_PROVENANCE" ;;
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
			sha := scheduledEnv("CI_DEVELOP_SHA")
			statusSHA := sha
			if tt.wrongSHA {
				statusSHA = strings.Repeat("b", 40)
			}
			auditState := tt.auditState
			if auditState == "" {
				auditState = "success"
			}
			statuses := []map[string]any{{"context": "scheduled-full-ci", "id": 123, "state": "success"}}
			if tt.newerFailure {
				statuses = append(statuses, map[string]any{"context": "scheduled-full-ci", "id": 124, "state": "failure"})
			}
			if !tt.missingAudit {
				statuses = append(statuses, map[string]any{"context": "release-security-audit", "id": 125, "state": auditState})
			}
			if tt.superseded {
				statuses = append(statuses, map[string]any{"context": "release-security-audit", "id": 126, "state": "failure"})
			}
			statuses = append(statuses, map[string]any{"context": "extra", "id": 127, "state": "success"})
			rawStatuses, err := json.Marshal([]any{map[string]any{"sha": statusSHA, "statuses": statuses}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "statuses"), rawStatuses, 0o600); err != nil {
				t.Fatal(err)
			}
			manifest := releaseprovenance.Manifest{Schema: 1, Repository: tt.repository, Tag: "v1.2.3", Commit: sha,
				Checks: []releaseprovenance.Check{{Name: "scheduled-full-ci", Status: "completed", Conclusion: "success", StatusID: 123}}}
			if !tt.oldProvenance {
				manifest.Checks = append(manifest.Checks, releaseprovenance.Check{Name: "release-security-audit", Status: "completed", Conclusion: "success", StatusID: 125})
			}
			annotation, err := releaseprovenance.Annotation(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "provenance"), []byte(annotation), 0o600); err != nil {
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
			command.Env = append(command.Env, "FIXTURE_STATUSES="+filepath.Join(dir, "statuses"), "FIXTURE_PROVENANCE="+filepath.Join(dir, "provenance"))
			for i := len(command.Env) - 1; i >= 0; i-- {
				if strings.HasPrefix(command.Env[i], "DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON=") {
					command.Env = append(command.Env[:i], command.Env[i+1:]...)
				}
			}
			if !tt.unset {
				command.Env = append(command.Env, "DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON="+tt.requirements)
			}
			if tt.rerun {
				command.Env = append(command.Env, "GITHUB_RUN_ATTEMPT=2")
			}
			suiteFailed := tt.failed || tt.missingJob || tt.skippedJob || tt.latestFailed || tt.wrongJobSHA
			skipped := tt.cancelledRun || tt.workflowCancelled
			output, err := command.CombinedOutput()
			if (err != nil) != ((suiteFailed || tt.unavailable || tt.cancelledJob || tt.timedOutJob || tt.newerFailure || tt.rejectReuse) && !skipped) {
				t.Fatalf("finalizer = %v: %s", err, output)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(calls)
			deleted := strings.Contains(text, "-X DELETE repos/"+tt.repository+"/actions/artifacts/7")
			wantDeleted := !tt.develop && (skipped || suiteFailed || tt.cancelledJob || tt.timedOutJob || tt.staleHead || tt.newerFailure || tt.rejectReuse)
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
			if tt.rejectReuse && !strings.Contains(string(output), "refusing to redispatch") {
				t.Fatalf("missing reuse rejection: %s", output)
			}
			if tt.awaitsStatus {
				if !strings.Contains(text, "native run ./tools/cifailure") {
					t.Fatalf("awaiting audit did not report validated suite: %s", text)
				}
				if !strings.Contains(string(output), "awaits required status release-security-audit") || !strings.Contains(text, "context=scheduled-full-ci") || strings.Contains(text, "tag -a ") || strings.Contains(text, "/dispatches") || deleted {
					t.Fatalf("waiting for audit produced incorrect effects: %s: %s", text, output)
				}
				if tt.auditThenRerun {
					statuses = append(statuses, map[string]any{"context": "release-security-audit", "id": 125, "state": "success"})
					updated, err := json.Marshal([]any{map[string]any{"sha": sha, "statuses": statuses}})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "statuses"), updated, 0o600); err != nil {
						t.Fatal(err)
					}
					again := exec.CommandContext(t.Context(), "bash", "scripts/scheduled-ci-finish.sh")
					again.Dir = command.Dir
					again.Env = append(command.Env, "GITHUB_RUN_ATTEMPT=2")
					if output, err := again.CombinedOutput(); err != nil {
						t.Fatalf("audit rerun = %v: %s", err, output)
					}
					calls, err := os.ReadFile(filepath.Join(dir, "calls"))
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(calls), "repository tag -a") || !strings.Contains(string(calls), "/dispatches") {
						t.Fatalf("green audited rerun did not release: %s", calls)
					}
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
				wantDispatch := !tt.releaseRun && !tt.newerFailure && !tt.rejectReuse
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
			} else if suiteFailed || tt.cancelledJob || tt.timedOutJob {
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
				message, err := os.ReadFile(filepath.Join(dir, "scheduled-ci-tag-message"))
				if err != nil {
					t.Fatal(err)
				}
				produced, err := releaseprovenance.FromTagMessage(string(message), tt.repository, tag, sha)
				if err != nil {
					t.Fatal(err)
				}
				evidence := releaseprovenance.GitHubEvidence{RequiredChecks: []releaseprovenance.RepositoryRequirement{{Name: "scheduled-full-ci"}, {Name: "release-security-audit"}}}
				if tt.requirements == `["extra"]` {
					evidence.RequiredChecks = append(evidence.RequiredChecks, releaseprovenance.RepositoryRequirement{Name: "extra"})
				}
				for _, status := range statuses {
					evidence.Checks = append(evidence.Checks, releaseprovenance.ObservedCheck{Name: status["context"].(string), Status: "completed", Conclusion: status["state"].(string), Commit: statusSHA, StatusID: int64(status["id"].(int))})
				}
				if err := releaseprovenance.VerifyGitHubEvidence(produced, evidence); err != nil {
					t.Fatalf("finalizer provenance rejected: %v", err)
				}
				for i, check := range produced.Checks {
					if check.Name == "release-security-audit" {
						produced.Checks = append(produced.Checks[:i], produced.Checks[i+1:]...)
						break
					}
				}
				if err := releaseprovenance.VerifyGitHubEvidence(produced, evidence); err == nil {
					t.Fatal("verifier accepted missing audit provenance")
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
