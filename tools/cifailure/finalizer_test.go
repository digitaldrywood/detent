package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	const repository = "example-org/example-project"
	for _, tt := range []struct {
		name, requirements, auditState, creators, lateContext            string
		awaitsStatus, unset, missingAudit, missingSuite, wrongSHA        bool
		superseded, suiteSuperseded, staleHead, newSeries                bool
		differentSeries, unchanged, releaseRun, oldProvenance            bool
		rejectReuse, badAllowlist, badOverride, failedExtra              bool
		moveDuringCheck, failDuringCheck, foreignCreator, missingCreator bool
		platform, override, recordedOverride, suiteState                 string
		actor, actorType, overrideActors                                 string
		local, noOverrideActors, rejectOverride, identityFailure         bool
	}{
		{name: "unauthorized actor cannot override", actor: "other-user", override: "Owner directs release", suiteState: "failure", rejectOverride: true},
		{name: "partial actor match cannot override", overrideActors: `["example-operator-extra"]`, override: "Owner directs release", rejectOverride: true},
		{name: "missing override allowlist denies everyone", noOverrideActors: true, override: "Owner directs release", rejectOverride: true},
		{name: "empty override allowlist denies everyone", overrideActors: `[]`, override: "Owner directs release", rejectOverride: true},
		{name: "malformed override allowlist is rejected", overrideActors: `{`, override: "Owner directs release", rejectOverride: true},
		{name: "bot actor rejected even when allowlisted", actor: "release[bot]", actorType: "Bot", overrideActors: `["release[bot]"]`, override: "Owner directs release", rejectOverride: true},
		{name: "automation account rejected even when allowlisted", actorType: "Bot", override: "Owner directs release", rejectOverride: true},
		{name: "identity lookup failure denies override", identityFailure: true, override: "Owner directs release", rejectOverride: true},
		{name: "allowlisted local operator can override", local: true, override: "Owner directs release", suiteState: "failure"},
		{name: "unauthorized local operator rejected", local: true, actor: "other-user", override: "Owner directs release", rejectOverride: true},
		{name: "local automation credential rejected", local: true, actorType: "Bot", override: "Owner directs release", rejectOverride: true},
		{name: "local missing repository allowlist denies override", local: true, noOverrideActors: true, override: "Owner directs release", rejectOverride: true},
		{name: "unauthorized actor cannot reuse recorded override", actor: "other-user", recordedOverride: "Owner previously accepted validation failure", suiteState: "failure", unchanged: true, rejectOverride: true},
		{name: "reuse recorded override without ambient input", recordedOverride: "Owner previously accepted failed validation --> investigate", suiteState: "failure", unchanged: true},
		{name: "reuse recorded override retains audit requirement", recordedOverride: "Owner previously accepted failed validation", auditState: "failure", unchanged: true, rejectReuse: true},
		{name: "ambient override cannot retrofit ordinary tag", override: "Owner directs release", suiteState: "failure", unchanged: true, rejectReuse: true},
		{name: "override still requires configured checks", override: "Owner directs release", requirements: `["extra"]`, failedExtra: true, awaitsStatus: true},
		{name: "blank override is rejected", override: " \t", badOverride: true},
		{name: "override reason preserves quotes and annotation suffix", override: "Owner accepts \"known\" failure --> investigate\nnext", suiteState: "failure"},
		{name: "override failed validation tags reason and URL", override: "Owner accepts known validation failure", suiteState: "failure"},
		{name: "override missing validation", override: "Owner accepts missing validation", missingSuite: true},
		{name: "override stale validation", override: "Owner accepts stale validation", suiteState: "stale"},
		{name: "override superseded validation", override: "Owner accepts superseded validation", suiteSuperseded: true},
		{name: "override still requires audit", override: "Owner directs release", missingAudit: true, awaitsStatus: true},
		{name: "override refuses failed audit", override: "Owner directs release", auditState: "failure", awaitsStatus: true},
		{name: "override refuses late audit failure", override: "Owner directs release", failDuringCheck: true, lateContext: "release-security-audit", awaitsStatus: true},
		{name: "override still requires platform", override: "Owner directs release", platform: "missing", awaitsStatus: true},
		{name: "failed validation without override", suiteState: "failure", awaitsStatus: true},
		{name: "green statuses tag and dispatch", requirements: `["scheduled-full-ci", "release-security-audit"]`},
		{name: "platform missing", platform: "missing", awaitsStatus: true},
		{name: "platform failed", platform: "failure", awaitsStatus: true},
		{name: "platform stale", platform: "stale", awaitsStatus: true},
		{name: "platform pending supersedes success", platform: "pending", awaitsStatus: true},
		{name: "platform future timestamp", platform: "future", awaitsStatus: true},
		{name: "platform ancestor succeeds", platform: "ancestor"},
		{name: "platform invalid timestamp", platform: "invalid", awaitsStatus: true},
		{name: "platform API failure", platform: "api-error", awaitsStatus: true},
		{name: "platform foreign creator refused", platform: "foreign", awaitsStatus: true},
		{name: "reuse missing platform", platform: "missing", unchanged: true, rejectReuse: true},
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
		{name: "head advances between check and tag", moveDuringCheck: true},
		{name: "new failure arrives between check and tag", failDuringCheck: true, awaitsStatus: true},
		{name: "new audit failure arrives between check and tag", failDuringCheck: true, lateContext: "release-security-audit", awaitsStatus: true},
		{name: "missing creator allowlist refused", badAllowlist: true},
		{name: "empty creator allowlist refused", creators: `[]`, badAllowlist: true},
		{name: "malformed creator allowlist refused", creators: `{`, badAllowlist: true},
		{name: "invalid creator allowlist entry refused", creators: `[null]`, badAllowlist: true},
		{name: "foreign success creator refused", foreignCreator: true, awaitsStatus: true},
		{name: "missing status creator refused", missingCreator: true, awaitsStatus: true},
		{name: "reuse foreign success creator refused", unchanged: true, foreignCreator: true, rejectReuse: true},
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
  'api user' | 'api users/'*)
    if [ "$FIXTURE_IDENTITY_FAILURE" = true ]; then exit 1; fi
    printf '{"login":"%s","type":"%s"}\n' "$FIXTURE_ACTOR" "$FIXTURE_ACTOR_TYPE" ;;
  *'/actions/variables/RELEASE_OVERRIDE_ACTORS'*)
    if [ "$FIXTURE_NO_OVERRIDE_ACTORS" = true ]; then exit 1; fi
    printf '%s\n' "$FIXTURE_OVERRIDE_ACTORS" ;;
  *"/commits/$CI_DEVELOP_SHA/statuses?per_page=100")
    if [ "$FIXTURE_PLATFORM_MODE" = api-error ]; then exit 1; fi
    if [ -f "$FIXTURE_STATUS_READ" ] && [ "$FIXTURE_FAIL_DURING_CHECK" = true ]; then
      cat "$FIXTURE_LATE_STATUSES"
    else
      cat "$FIXTURE_STATUSES"
    fi
    if [ -f "$FIXTURE_HEAD_READ" ]; then touch "$FIXTURE_STATUS_READ"; fi
    touch "$FIXTURE_HEAD_READ" ;;
  *'/commits/'*'/statuses?per_page=100')
    if [ "$FIXTURE_PLATFORM_MODE" = api-error ]; then exit 1; fi
    if [[ "$*" == *"/$FIXTURE_PLATFORM_SHA/"* ]]; then cat "$FIXTURE_PLATFORM"; else printf '[[]]'; fi ;;
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
  'rev-list '*) printf '%s\n%s\n' "$FIXTURE_HEAD" bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
  'ls-remote '*)
    if [ -f "$FIXTURE_STATUS_READ" ] && [ "$FIXTURE_MOVE_DURING_CHECK" = true ]; then
      printf 'advanced\trefs/heads/release/v1.2\n'
    else
      printf '%s\trefs/heads/release/v1.2\n' "$FIXTURE_HEAD"
    fi ;;
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
			suiteState := tt.suiteState
			if suiteState == "" {
				suiteState = "success"
			}
			var statuses []map[string]any
			if !tt.missingSuite {
				statuses = append(statuses, map[string]any{"context": "scheduled-full-ci", "id": 123, "state": suiteState, "target_url": "https://example.com/validation/123"})
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
			extraState := "success"
			if tt.failedExtra {
				extraState = "failure"
			}
			statuses = append(statuses, map[string]any{"context": "extra", "id": 127, "state": extraState})
			for _, status := range statuses {
				if tt.missingCreator {
					continue
				}
				creator := "example-operator"
				if tt.foreignCreator {
					creator = "foreign-producer"
				}
				status["creator"] = map[string]any{"login": creator}
			}
			if tt.foreignCreator {
				statuses = append(statuses,
					map[string]any{"context": "scheduled-full-ci", "id": 121, "state": "success", "creator": map[string]any{"login": "example-operator"}},
					map[string]any{"context": "release-security-audit", "id": 122, "state": "success", "creator": map[string]any{"login": "example-operator"}})
			}
			lateContext := tt.lateContext
			if lateContext == "" {
				lateContext = "scheduled-full-ci"
			}
			lateStatuses := append(append([]map[string]any{}, statuses...), map[string]any{"context": lateContext, "id": 128, "state": "failure", "creator": map[string]any{"login": "example-operator"}})
			platformSHA := sha
			if tt.platform == "ancestor" {
				platformSHA = strings.Repeat("b", 40)
			}
			platformState := "success"
			platformTime := time.Now().UTC().Add(-time.Hour)
			switch tt.platform {
			case "failure", "pending":
				platformState = tt.platform
			case "stale":
				platformTime = platformTime.Add(-24 * time.Hour)
			case "future":
				platformTime = platformTime.Add(2 * time.Hour)
			}
			platformCreator := "example-operator"
			if tt.platform == "foreign" {
				platformCreator = "foreign-producer"
			}
			platform := []map[string]any{{"context": "platform-full-ci", "id": 200, "state": platformState, "created_at": platformTime.Format(time.RFC3339), "creator": map[string]any{"login": platformCreator}}}
			if tt.platform == "invalid" {
				platform[0]["created_at"] = "invalid"
			}
			if tt.platform == "missing" {
				platform = []map[string]any{}
			}
			if tt.platform == "pending" {
				platform = append(platform, map[string]any{"context": "platform-full-ci", "id": 199, "state": "success", "created_at": platformTime.Format(time.RFC3339), "creator": map[string]any{"login": "example-operator"}})
			}
			rawPlatform, err := json.Marshal([]any{platform})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "platform"), rawPlatform, 0o600); err != nil {
				t.Fatal(err)
			}
			var headPlatform []map[string]any
			if platformSHA == sha {
				headPlatform = platform
			}
			headStatuses := func(listed []map[string]any) []map[string]any {
				if statusSHA != sha {
					return headPlatform
				}
				return append(append([]map[string]any{}, listed...), headPlatform...)
			}
			lateRaw, err := json.Marshal([]any{headStatuses(lateStatuses)})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "late-statuses"), lateRaw, 0o600); err != nil {
				t.Fatal(err)
			}
			rawStatuses, err := json.Marshal([]any{headStatuses(statuses)})
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
			if tt.recordedOverride != "" {
				manifest.Checks = manifest.Checks[1:]
				manifest.ValidationOverride = &releaseprovenance.ValidationOverride{Reason: tt.recordedOverride, Status: "failure", StatusID: 123, StatusURL: "https://example.com/validation/123"}
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
			actor := tt.actor
			if actor == "" {
				actor = "example-operator"
			}
			actorType := tt.actorType
			if actorType == "" {
				actorType = "User"
			}
			overrideActors := tt.overrideActors
			if overrideActors == "" && !tt.noOverrideActors {
				overrideActors = `["example-operator"]`
			}
			command := exec.CommandContext(t.Context(), "bash", "scripts/release-finalize.sh")
			command.Dir = root
			command.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir,
				"FIXTURE_STATUS_READ=" + filepath.Join(dir, "status-read"), "FIXTURE_HEAD_READ=" + filepath.Join(dir, "head-read"),
				"FIXTURE_LATE_STATUSES=" + filepath.Join(dir, "late-statuses"),
				"FIXTURE_MOVE_DURING_CHECK=" + flag[tt.moveDuringCheck], "FIXTURE_FAIL_DURING_CHECK=" + flag[tt.failDuringCheck],
				"GITHUB_REPOSITORY=" + repository, "CI_DEVELOP_SHA=" + sha, "CI_RELEASE_BRANCH=" + releaseBranch,
				"FIXTURE_LOG=" + filepath.Join(dir, "calls"), "FIXTURE_HEAD=" + head, "FIXTURE_NEW_SERIES=" + flag[tt.newSeries],
				"FIXTURE_UNCHANGED=" + flag[tt.unchanged], "FIXTURE_RELEASE_RUN=" + flag[tt.releaseRun],
				"FIXTURE_PLATFORM_MODE=" + tt.platform, "FIXTURE_PLATFORM_SHA=" + platformSHA, "FIXTURE_PLATFORM=" + filepath.Join(dir, "platform"),
				"FIXTURE_STATUSES=" + filepath.Join(dir, "statuses"), "FIXTURE_PROVENANCE=" + filepath.Join(dir, "provenance")}
			creators := tt.creators
			if creators == "" && !tt.badAllowlist {
				creators = `["example-operator"]`
			}
			if creators != "" {
				command.Env = append(command.Env, "DETENT_RELEASE_STATUS_CREATORS_JSON="+creators)
			}
			if !tt.unset {
				command.Env = append(command.Env, "DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON="+tt.requirements)
			}
			command.Env = append(command.Env, "DETENT_RELEASE_OVERRIDE="+tt.override)
			command.Env = append(command.Env, "FIXTURE_ACTOR="+actor, "FIXTURE_ACTOR_TYPE="+actorType,
				"FIXTURE_OVERRIDE_ACTORS="+overrideActors, "FIXTURE_NO_OVERRIDE_ACTORS="+flag[tt.noOverrideActors],
				"FIXTURE_IDENTITY_FAILURE="+flag[tt.identityFailure])
			if !tt.local {
				command.Env = append(command.Env, "GITHUB_ACTIONS=true", "GITHUB_ACTOR="+actor)
				if !tt.noOverrideActors {
					command.Env = append(command.Env, "RELEASE_OVERRIDE_ACTORS="+overrideActors)
				}
			}
			output, err := command.CombinedOutput()
			if (err != nil) != (tt.rejectReuse || tt.badAllowlist || tt.badOverride || tt.rejectOverride) {
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
			case tt.rejectOverride:
				if !strings.Contains(string(output), "Release override requires an authorized human") || strings.Contains(text, "tag -a ") || strings.Contains(text, "push ") || strings.Contains(text, "/dispatches") {
					t.Fatalf("unauthorized override produced release effects: %s: %s", text, output)
				}
			case tt.badOverride:
				if !strings.Contains(string(output), "override requires a nonempty reason") || strings.Contains(text, "tag -a ") || strings.Contains(text, "/dispatches") {
					t.Fatalf("invalid override produced release effects: %s: %s", text, output)
				}
			case tt.badAllowlist:
				if !strings.Contains(string(output), "jq:") || strings.Contains(text, "tag -a ") || strings.Contains(text, "/dispatches") {
					t.Fatalf("invalid allowlist produced release effects: %s: %s", text, output)
				}
			case tt.rejectReuse:
				if !strings.Contains(string(output), "refusing to redispatch") || strings.Contains(text, "/dispatches") {
					t.Fatalf("reuse of mismatched provenance: %s: %s", text, output)
				}
			case tt.awaitsStatus:
				if !strings.Contains(string(output), "awaits required statuses") || strings.Contains(text, "tag -a ") || strings.Contains(text, "/dispatches") {
					t.Fatalf("missing evidence produced release effects: %s: %s", text, output)
				}
			case tt.staleHead || tt.moveDuringCheck:
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
				if tt.override != "" {
					recorded := produced.ValidationOverride
					if recorded == nil || recorded.Reason != tt.override {
						t.Fatalf("override reason lost: %s", message)
					}
					summary := strings.Split(string(message), "\n<!-- detent-release-provenance:")[0]
					if !strings.Contains(summary, "with full-validation override") || !strings.Contains(summary, "Owner override reason:") {
						t.Fatalf("override missing from visible tag message: %s", message)
					}
					if !tt.missingSuite && !tt.suiteSuperseded && !strings.Contains(summary, "https://example.com/validation/123") {
						t.Fatalf("validation status URL missing from visible tag message: %s", message)
					}
					if !tt.missingSuite && !tt.suiteSuperseded && (recorded.Status != suiteState || recorded.StatusID != 123 || recorded.StatusURL != "https://example.com/validation/123") {
						t.Fatalf("override validation evidence lost: %s", message)
					}
					if tt.missingSuite && (recorded.Status != "missing" || recorded.StatusID != 0 || recorded.StatusURL != "") {
						t.Fatalf("missing validation was misrepresented: %s", message)
					}
				} else if produced.ValidationOverride != nil {
					t.Fatalf("ordinary release acquired an override: %s", message)
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
