package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	provenance "github.com/digitaldrywood/detent/internal/releaseprovenance"
)

func TestRun(t *testing.T) {
	t.Parallel()

	const commit = "0123456789abcdef0123456789abcdef01234567"
	manifest := provenance.Manifest{
		Schema:     provenance.Schema,
		Repository: "digitaldrywood/detent",
		Tag:        "v1.2.3",
		Commit:     commit,
		Checks:     []provenance.Check{{Name: "CI", Status: "completed", Conclusion: "success", CheckRunID: 101}},
	}
	annotation, err := provenance.Annotation(manifest)
	if err != nil {
		t.Fatalf("Annotation() error = %v", err)
	}
	forged := manifest
	forged.Checks = []provenance.Check{{Name: "Fabricated", Status: "completed", Conclusion: "success", CheckRunID: 999}}
	forgedAnnotation, err := provenance.Annotation(forged)
	if err != nil {
		t.Fatalf("Annotation(forged) error = %v", err)
	}
	omittedIDAnnotation := strings.Replace(annotation, `,"check_run_id":101`, "", 1)
	olderSuccess := manifest
	olderSuccess.Checks = []provenance.Check{{Name: "CI", Status: "completed", Conclusion: "success", CheckRunID: 100}}
	olderSuccessAnnotation, err := provenance.Annotation(olderSuccess)
	if err != nil {
		t.Fatalf("Annotation(older success) error = %v", err)
	}
	configured := manifest
	configured.Checks = append(configured.Checks, provenance.Check{Name: "Security", Status: "completed", Conclusion: "success", CheckRunID: 201})
	configuredAnnotation, err := provenance.Annotation(configured)
	if err != nil {
		t.Fatalf("Annotation(configured) error = %v", err)
	}
	legacyStatus := manifest
	legacyStatus.Checks = []provenance.Check{{Name: "Legacy", Status: "completed", Conclusion: "success", StatusID: 301}}
	legacyStatusAnnotation, err := provenance.Annotation(legacyStatus)
	if err != nil {
		t.Fatalf("Annotation(legacy status) error = %v", err)
	}

	tests := []struct {
		name           string
		message        string
		commit         string
		checks         string
		statuses       string
		rulesets       string
		requiredChecks string
		wantErr        string
	}{
		{name: "valid", message: annotation, commit: commit},
		{name: "missing evidence", message: "manual tag", commit: commit, wantErr: "missing provenance annotation"},
		{name: "wrong candidate", message: annotation, commit: "89abcdef0123456789abcdef0123456789abcdef", wantErr: "want"},
		{name: "fabricated successful check", message: forgedAnnotation, commit: commit, wantErr: "omits authenticated repository requirement"},
		{name: "missing immutable evidence ID", message: omittedIDAnnotation, commit: commit, wantErr: "missing an immutable evidence ID"},
		{
			name:    "older success does not hide newer failed check run",
			message: olderSuccessAnnotation,
			commit:  commit,
			checks:  `{"total_count":2,"check_runs":[{"id":100,"name":"CI","head_sha":"` + commit + `","status":"completed","conclusion":"success","app":{"id":15368}},{"id":101,"name":"CI","head_sha":"` + commit + `","status":"completed","conclusion":"failure","app":{"id":15368}}]}`,
			wantErr: "newer authenticated check run",
		},
		{
			name:           "configured release-only check",
			message:        configuredAnnotation,
			commit:         commit,
			requiredChecks: `["Security"]`,
			checks:         `{"total_count":2,"check_runs":[{"id":101,"name":"CI","head_sha":"` + commit + `","status":"completed","conclusion":"success","app":{"id":15368}},{"id":201,"name":"Security","head_sha":"` + commit + `","status":"completed","conclusion":"success","app":{"id":15368}}]}`,
		},
		{
			name:           "annotation omits configured release-only check",
			message:        annotation,
			commit:         commit,
			requiredChecks: `["Security"]`,
			checks:         `{"total_count":2,"check_runs":[{"id":101,"name":"CI","head_sha":"` + commit + `","status":"completed","conclusion":"success","app":{"id":15368}},{"id":201,"name":"Security","head_sha":"` + commit + `","status":"completed","conclusion":"success","app":{"id":15368}}]}`,
			wantErr:        "omits authenticated repository requirement \"Security\"",
		},
		{
			name:     "older success does not hide newer failed commit status",
			message:  legacyStatusAnnotation,
			commit:   commit,
			checks:   `{"total_count":0,"check_runs":[]}`,
			statuses: `{"sha":"` + commit + `","total_count":2,"statuses":[{"id":301,"context":"Legacy","state":"success"},{"id":302,"context":"Legacy","state":"failure"}]}`,
			rulesets: `{"enforcement":"active","target":"branch","conditions":{"ref_name":{"include":["~DEFAULT_BRANCH"],"exclude":[]}},"rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"Legacy","integration_id":0}]}}]}`,
			wantErr:  "newer authenticated commit status",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			messagePath := filepath.Join(dir, "tag-message")
			checkRunsPath := filepath.Join(dir, "check-runs.json")
			statusesPath := filepath.Join(dir, "statuses.json")
			rulesetsPath := filepath.Join(dir, "rulesets.json")
			outputPath := filepath.Join(dir, "out", "detent_release_provenance.json")
			if err := os.WriteFile(messagePath, []byte(tt.message), 0o600); err != nil {
				t.Fatalf("WriteFile(tag message) error = %v", err)
			}
			checkRuns := tt.checks
			if checkRuns == "" {
				checkRuns = `{"total_count":1,"check_runs":[{"id":101,"name":"CI","head_sha":"` + commit + `","status":"completed","conclusion":"success","app":{"id":15368}}]}`
			}
			statuses := tt.statuses
			if statuses == "" {
				statuses = `{"sha":"` + commit + `","total_count":0,"statuses":[]}`
			}
			rulesets := tt.rulesets
			if rulesets == "" {
				rulesets = `{"enforcement":"active","target":"branch","conditions":{"ref_name":{"include":["~DEFAULT_BRANCH"],"exclude":[]}},"rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"CI","integration_id":15368}]}}]}`
			}
			requiredChecks := tt.requiredChecks
			if requiredChecks == "" {
				requiredChecks = `[]`
			}
			for path, raw := range map[string]string{checkRunsPath: checkRuns, statusesPath: statuses, rulesetsPath: rulesets} {
				if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
					t.Fatalf("WriteFile(%s) error = %v", filepath.Base(path), err)
				}
			}
			var stderr bytes.Buffer
			err := run([]string{
				"-repository", manifest.Repository,
				"-tag", manifest.Tag,
				"-commit", tt.commit,
				"-default-branch-ref", "refs/heads/main",
				"-tag-message", messagePath,
				"-github-check-runs", checkRunsPath,
				"-github-statuses", statusesPath,
				"-github-rulesets", rulesetsPath,
				"-required-check-names-json", requiredChecks,
				"-output", outputPath,
			}, &stderr)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("run() error = %v, want containing %q", err, tt.wantErr)
				}
				if _, statErr := os.Stat(outputPath); !os.IsNotExist(statErr) {
					t.Fatalf("Stat(output) error = %v, want not exist", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("run() error = %v", err)
			}
			raw, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatalf("ReadFile(output) error = %v", err)
			}
			got, err := provenance.Parse(raw, manifest.Repository, manifest.Tag, manifest.Commit)
			if err != nil || got.Commit != manifest.Commit {
				t.Fatalf("output provenance = %#v, error = %v", got, err)
			}
		})
	}
}

func TestRunRequiresAuthoritativeReleaseOnlyPolicy(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	err := run([]string{
		"-tag-message", "tag-message",
		"-github-check-runs", "check-runs.json",
		"-github-statuses", "statuses.json",
		"-github-rulesets", "rulesets.json",
		"-output", "output.json",
	}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "-required-check-names-json is required") {
		t.Fatalf("run() error = %v, want required authoritative policy", err)
	}
}

func TestParseRequiredCheckNames(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		raw     string
		want    []string
		wantErr string
	}{
		{name: "explicit empty", raw: `[]`, want: []string{}},
		{name: "trimmed names", raw: `[" Security "]`, want: []string{"Security"}},
		{name: "null", raw: `null`, wantErr: "must be a JSON array"},
		{name: "not an array", raw: `{}`, wantErr: "cannot unmarshal"},
		{name: "blank name", raw: `[""]`, wantErr: "is blank"},
		{name: "duplicate name", raw: `["CI","CI"]`, wantErr: "is duplicated"},
		{name: "multiple values", raw: `[] []`, wantErr: "multiple JSON values"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseRequiredCheckNames(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseRequiredCheckNames() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("parseRequiredCheckNames() = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestRulesetAppliesToDefaultBranch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		include     []string
		exclude     []string
		wantApplies bool
	}{
		{name: "default branch alias", include: []string{"~DEFAULT_BRANCH"}, wantApplies: true},
		{name: "exact default branch", include: []string{"refs/heads/main"}, wantApplies: true},
		{name: "other branch", include: []string{"refs/heads/release"}},
		{name: "excluded default branch", include: []string{"~ALL"}, exclude: []string{"refs/heads/main"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var ruleset rulesetResponse
			ruleset.Conditions.RefName.Include = tt.include
			ruleset.Conditions.RefName.Exclude = tt.exclude
			if got := rulesetAppliesToDefaultBranch(ruleset, "refs/heads/main"); got != tt.wantApplies {
				t.Fatalf("rulesetAppliesToDefaultBranch() = %t, want %t", got, tt.wantApplies)
			}
		})
	}
}
