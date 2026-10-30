package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/releaseprovenance"
)

func TestReleaseFinalizer(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required for the release shell fixture")
	}
	const repository = "digitaldrywood/detent"
	for _, tt := range []struct {
		name, requirements, auditState                            string
		awaitsStatus, unset, missingAudit, missingSuite, wrongSHA bool
		superseded, suiteSuperseded, staleHead, newSeries         bool
		differentSeries, unchanged, releaseRun, oldProvenance     bool
		rejectReuse                                               bool
	}{
		{name: "green statuses tag and dispatch", requirements: `["scheduled-full-ci", "release-security-audit"]`},
		{name: "configured extras recorded", requirements: `["extra"]`},
		{name: "unset requirements still require audit", unset: true, missingAudit: true, awaitsStatus: true},
		{name: "empty requirements still require audit", requirements: `[]`, missingAudit: true, awaitsStatus: true},
		{name: "audit missing", missingAudit: true, awaitsStatus: true},
		{name: "audit failed", auditState: "failure", awaitsStatus: true},
		{name: "audit superseded by failure", superseded: true, awaitsStatus: true},
		{name: "local validation missing", missingSuite: true, awaitsStatus: true},
		{name: "local validation superseded by failure", suiteSuperseded: true, awaitsStatus: true},
		{name: "statuses on another SHA", wrongSHA: true, awaitsStatus: true},
		{name: "moved release head does not tag", staleHead: true},
		{name: "new release series starts at patch zero", newSeries: true},
		{name: "older series tag does not suppress new series", newSeries: true, unchanged: true, differentSeries: true},
		{name: "missing release dispatch recovered without recreating tag", unchanged: true},
		{name: "existing release workflow is not redispatched", unchanged: true, releaseRun: true},
		{name: "reuse missing audit", unchanged: true, missingAudit: true, rejectReuse: true},
		{name: "reuse old provenance", unchanged: true, oldProvenance: true, rejectReuse: true},
		{name: "reuse failed audit", unchanged: true, auditState: "failure", rejectReuse: true},
		{name: "reuse superseded validation", unchanged: true, suiteSuperseded: true, rejectReuse: true},
		{name: "reuse another SHA", unchanged: true, wrongSHA: true, rejectReuse: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			programs := map[string]string{
				"gh": `#!/usr/bin/env bash
set -euo pipefail
printf 'forge %s\n' "$*" >> "$FIXTURE_LOG"
case "$*" in
  *'/commits/'*'/status?per_page=100') cat "$FIXTURE_STATUSES" ;;
  *'/workflows/release.yml/runs?'*)
    if [ "$FIXTURE_RELEASE_RUN" = true ]; then
      printf '[{"workflow_runs":[{"head_branch":"v1.2.3"}]}]\n'
    else
      printf '[{"workflow_runs":[{"head_branch":"v1.1.9"}]}]\n'
    fi ;;
  *'/dispatches'*) ;;
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
			var statuses []map[string]any
			if !tt.missingSuite {
				statuses = append(statuses, map[string]any{"context": "scheduled-full-ci", "id": 123, "state": "success"})
			}
			if tt.suiteSuperseded {
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
			manifest := releaseprovenance.Manifest{Schema: 1, Repository: repository, Tag: "v1.2.3", Commit: sha,
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
			releaseBranch := "release/v1.2"
			if tt.differentSeries {
				releaseBranch = "release/v1.3"
			}
			head := sha
			if tt.staleHead {
				head = strings.Repeat("b", 40)
			}
			flag := map[bool]string{true: "true", false: "false"}
			command := exec.CommandContext(t.Context(), "bash", "scripts/release-finalize.sh")
			command.Dir = root
			command.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir,
				"GITHUB_REPOSITORY=" + repository, "CI_DEVELOP_SHA=" + sha, "CI_RELEASE_BRANCH=" + releaseBranch,
				"FIXTURE_LOG=" + filepath.Join(dir, "calls"), "FIXTURE_HEAD=" + head, "FIXTURE_NEW_SERIES=" + flag[tt.newSeries],
				"FIXTURE_UNCHANGED=" + flag[tt.unchanged], "FIXTURE_RELEASE_RUN=" + flag[tt.releaseRun],
				"FIXTURE_STATUSES=" + filepath.Join(dir, "statuses"), "FIXTURE_PROVENANCE=" + filepath.Join(dir, "provenance")}
			if !tt.unset {
				command.Env = append(command.Env, "DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON="+tt.requirements)
			}
			output, err := command.CombinedOutput()
			if (err != nil) != tt.rejectReuse {
				t.Fatalf("finalizer = %v: %s", err, output)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(calls)
			if strings.Contains(text, "/statuses/") {
				t.Fatalf("finalizer posted a status: %s", text)
			}
			switch {
			case tt.rejectReuse:
				if !strings.Contains(string(output), "refusing to redispatch") || strings.Contains(text, "/dispatches") {
					t.Fatalf("reuse of mismatched provenance: %s: %s", text, output)
				}
			case tt.awaitsStatus:
				if !strings.Contains(string(output), "awaits required statuses") || strings.Contains(text, "tag -a ") || strings.Contains(text, "/dispatches") {
					t.Fatalf("missing evidence produced release effects: %s: %s", text, output)
				}
			case tt.staleHead:
				if strings.Contains(text, "tag -a ") || strings.Contains(text, "/dispatches") {
					t.Fatalf("moved head published a release: %s", text)
				}
			case tt.unchanged && !tt.differentSeries:
				if strings.Contains(text, "tag -a ") {
					t.Fatalf("existing release tag was replaced: %s", text)
				}
				if strings.Contains(text, "/dispatches") == tt.releaseRun {
					t.Fatalf("release dispatch recovery wrong (existing run %t): %s", tt.releaseRun, text)
				}
			default:
				tag := "v1.2.4"
				if tt.newSeries {
					tag = "v1.2.0"
				}
				if tt.differentSeries {
					tag = "v1.3.0"
				}
				message, err := os.ReadFile(filepath.Join(dir, "release-tag-message"))
				if err != nil {
					t.Fatal(err)
				}
				produced, err := releaseprovenance.FromTagMessage(string(message), repository, tag, sha)
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
				if !strings.Contains(text, "repository tag -a "+tag+" "+sha) || !strings.Contains(text, "refs/tags/"+tag) || !strings.Contains(text, "/release.yml/dispatches") {
					t.Fatalf("release tag evidence lost: %s", text)
				}
			}
		})
	}
}
