package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/release"
	provenance "github.com/digitaldrywood/detent/internal/releaseprovenance"
)

// Exercise the coordinator's annotation through the release workflow's signing
// input gate. Read the actual triggers: another CI run on tag push invalidates
// the immutable check IDs selected before CreateTag.
func TestCoordinatorTagToSigningProvenance(t *testing.T) {
	t.Parallel()
	const commit = "0123456789abcdef0123456789abcdef01234567"
	for _, tt := range []struct {
		name, status, conclusion string
		newer, missing, wantErr  bool
	}{
		{name: "main checks remain valid after tag"},
		{name: "independent successful rerun makes annotation stale", newer: true, status: "completed", conclusion: "success", wantErr: true},
		{name: "independent pending rerun", newer: true, status: "in_progress", wantErr: true},
		{name: "cancelled evidence", status: "completed", conclusion: "cancelled", wantErr: true},
		{name: "skipped evidence", status: "completed", conclusion: "skipped", wantErr: true},
		{name: "failed evidence", status: "completed", conclusion: "failure", wantErr: true},
		{name: "missing evidence", missing: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			backend := &sequenceBackend{repo: release.Repository{
				Name: "digitaldrywood/detent", HeadSHA: commit, LatestTag: "v1.0.0", LatestSHA: "previous", TaggedAt: now.Add(-time.Hour),
				RequiredCheckNames: []string{"Lint"},
				Commits:            []release.Commit{{SHA: commit, Message: "fix: release provenance", MergedAt: now, IssueRefs: []string{"digitaldrywood/detent#2419"}}},
				Checks:             []release.Check{{Name: "Lint", SHA: commit, Status: "completed", Conclusion: "success", CheckRunID: 101}},
			}}
			coordinator := release.New(release.Config{Enabled: true, RequireGreenCI: true, MinMergedIssues: 1, VersionBump: "patch"}, backend)
			status, _ := coordinator.Evaluate(t.Context(), now)
			if backend.tag.Name == "" {
				t.Fatalf("coordinator did not create tag: %#v", status)
			}
			if !workflowPushesTag(t, "release.yml", backend.tag.Name) {
				t.Fatal("tag does not trigger signing workflow")
			}
			tagCI := workflowPushesTag(t, "ci.yml", backend.tag.Name)
			check := func(id int, status, conclusion string) string {
				return fmt.Sprintf(`{"id":%d,"name":"Lint","head_sha":%q,"status":%q,"conclusion":%q,"app":{"id":15368}}`, id, commit, status, conclusion)
			}
			checks := []string{check(101, "completed", "success")}
			if tagCI || tt.newer {
				checks = append(checks, check(102, "completed", "success"))
			}
			if tt.status != "" {
				id := 101
				if tt.newer || tagCI {
					id = 102
				}
				checks[len(checks)-1] = check(id, tt.status, tt.conclusion)
			}
			if tt.missing {
				checks = nil
			}
			dir := t.TempDir()
			inputs := map[string]string{
				"tag":      backend.tag.Message,
				"checks":   fmt.Sprintf(`{"total_count":%d,"check_runs":[%s]}`, len(checks), strings.Join(checks, ",")),
				"statuses": `{"sha":"` + commit + `","total_count":0,"statuses":[]}`,
				"rulesets": `{"enforcement":"active","target":"branch","conditions":{"ref_name":{"include":["~DEFAULT_BRANCH"]}},"rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"Lint","integration_id":15368}]}}]}`,
			}
			for name, raw := range inputs {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(dir, "detent_release_provenance.json")
			// A signing-workflow retry must preserve the exact same valid annotation.
			for range 2 {
				err := run([]string{"-repository", backend.repo.Name, "-tag", backend.tag.Name, "-commit", backend.tag.SHA, "-default-branch-ref", "refs/heads/main", "-tag-message", filepath.Join(dir, "tag"), "-github-check-runs", filepath.Join(dir, "checks"), "-github-statuses", filepath.Join(dir, "statuses"), "-github-rulesets", filepath.Join(dir, "rulesets"), "-required-check-names-json", "[]", "-output", output}, io.Discard)
				if (err != nil) != tt.wantErr {
					t.Fatalf("tag-to-sign gate = %v, want error %v (tag triggers duplicate CI: %v)", err, tt.wantErr, tagCI)
				}
				if tt.wantErr {
					if _, err := os.Stat(output); !os.IsNotExist(err) {
						t.Fatalf("rejected evidence produced signing input: %v", err)
					}
					continue
				}
				raw, err := os.ReadFile(output)
				if err != nil {
					t.Fatal(err)
				}
				manifest, err := provenance.Parse(raw, backend.repo.Name, backend.tag.Name, commit)
				if err != nil || len(manifest.Checks) != 1 || manifest.Checks[0].CheckRunID != 101 {
					t.Fatalf("signing input = %#v, %v", manifest, err)
				}
			}
		})
	}
}

func workflowPushesTag(t *testing.T, filename, tag string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", filename))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			Push struct {
				Branches []string `yaml:"branches"`
				Tags     []string `yaml:"tags"`
			} `yaml:"push"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	if filename == "ci.yml" && !slices.Contains(workflow.On.Push.Branches, "main") {
		t.Fatal("main push must still trigger mandatory CI")
	}
	if len(workflow.On.Push.Branches) == 0 && len(workflow.On.Push.Tags) == 0 {
		return true
	}
	for _, pattern := range workflow.On.Push.Tags {
		matched, err := filepath.Match(pattern, tag)
		if err != nil {
			t.Fatal(err)
		}
		if matched {
			return true
		}
	}
	return false
}

type sequenceBackend struct {
	repo release.Repository
	tag  release.Tag
}

func (b *sequenceBackend) Inspect(context.Context) (release.Repository, error) { return b.repo, nil }
func (b *sequenceBackend) CreateTag(_ context.Context, tag release.Tag) error {
	b.tag = tag
	return nil
}
func (*sequenceBackend) ReleaseWorkflow(context.Context, string) (release.WorkflowRun, bool, error) {
	return release.WorkflowRun{}, false, nil
}
func (*sequenceBackend) RerunFailedChecks(context.Context, []release.Check) error {
	return errors.New("unexpected rerun")
}
func (*sequenceBackend) EnsureReleaseReport(context.Context, release.Report) (bool, error) {
	return true, nil
}
