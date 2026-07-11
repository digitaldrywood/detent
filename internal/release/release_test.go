package release

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestNextVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		latest  string
		commits []Commit
		want    string
	}{
		{name: "feature bumps minor", latest: "v1.4.7", commits: []Commit{{Message: "feat(api): add cadence"}, {Message: "fix: retry status"}}, want: "v1.5.0"},
		{name: "fix bumps patch", latest: "v1.4.7", commits: []Commit{{Message: "fix: retry status"}}, want: "v1.4.8"},
		{name: "chore bumps patch", latest: "v1.4.7", commits: []Commit{{Message: "chore: update workflow"}}, want: "v1.4.8"},
		{name: "first feature release", commits: []Commit{{Message: "feat: initial release"}}, want: "v0.1.0"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := NextVersion(test.latest, test.commits)
			if err != nil {
				t.Fatalf("NextVersion() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("NextVersion() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestServiceDisabledDoesNothing(t *testing.T) {
	t.Parallel()

	backend := &fakeBackend{}
	service := New(Config{}, backend)
	status, decision := service.Evaluate(context.Background(), time.Now())
	if status.Enabled || status.State != "disabled" {
		t.Fatalf("Evaluate() status = %#v, want disabled", status)
	}
	if decision != (Decision{}) {
		t.Fatalf("Evaluate() decision = %#v, want empty", decision)
	}
	if backend.inspectCalls != 0 {
		t.Fatalf("Inspect() calls = %d, want 0", backend.inspectCalls)
	}
}

func TestServiceCountTriggerCreatesOneTagAndObservesRelease(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 10, 20, 0, 0, 0, time.UTC)
	backend := &fakeBackend{repo: Repository{
		Name:      "example/repo",
		HeadSHA:   "head",
		LatestTag: "v1.2.3",
		LatestSHA: "previous",
		TaggedAt:  now.Add(-time.Hour),
		Commits: []Commit{
			{SHA: "one", Message: "feat: add release", MergedAt: now.Add(-30 * time.Minute), IssueRefs: []string{"example/repo#1"}},
			{SHA: "two", Message: "fix: harden release", MergedAt: now.Add(-20 * time.Minute), IssueRefs: []string{"example/repo#2"}},
		},
		Checks: []Check{{Name: "CI", Status: "completed", Conclusion: "success"}},
	}, workflow: WorkflowRun{ID: 7, Status: "completed", Conclusion: "success"}, workflowFound: true}
	service := New(Config{Enabled: true, MinMergedIssues: 2, MaxAge: 24 * time.Hour, RequireGreenCI: true, VersionBump: "auto"}, backend)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			service.Evaluate(context.Background(), now)
		}()
	}
	wg.Wait()

	backend.mu.Lock()
	if len(backend.tags) != 1 {
		backend.mu.Unlock()
		t.Fatalf("created tags = %#v, want exactly one", backend.tags)
	}
	if backend.tags[0].Name != "v1.3.0" {
		backend.mu.Unlock()
		t.Fatalf("tag name = %q, want v1.3.0", backend.tags[0].Name)
	}
	backend.mu.Unlock()

	status, _ := service.Evaluate(context.Background(), now.Add(time.Minute))
	if status.State != "released" || status.LastRelease != "v1.3.0" || status.UnreleasedMerges != 0 {
		t.Fatalf("released status = %#v", status)
	}
}

func TestServiceRedCIFilesOneIssueAndNoTag(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 10, 20, 0, 0, 0, time.UTC)
	backend := &fakeBackend{repo: Repository{
		Name:      "example/repo",
		HeadSHA:   "head",
		LatestTag: "v1.2.3",
		LatestSHA: "previous",
		Commits:   []Commit{{Message: "fix: broken", MergedAt: now.Add(-time.Hour), IssueRefs: []string{"example/repo#1"}}},
		Checks:    []Check{{Name: "CI", Status: "completed", Conclusion: "failure"}},
	}}
	service := New(Config{Enabled: true, MinMergedIssues: 1, MaxAge: 24 * time.Hour, RequireGreenCI: true}, backend)

	service.Evaluate(context.Background(), now)
	service.Evaluate(context.Background(), now.Add(time.Minute))

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.tags) != 0 {
		t.Fatalf("created tags = %#v, want none", backend.tags)
	}
	if len(backend.failures) != 1 {
		t.Fatalf("failure issues = %#v, want exactly one", backend.failures)
	}
}

func TestServiceAgeTrigger(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 10, 20, 0, 0, 0, time.UTC)
	backend := &fakeBackend{repo: Repository{
		Name:      "example/repo",
		HeadSHA:   "head",
		LatestTag: "v1.2.3",
		LatestSHA: "previous",
		Commits:   []Commit{{Message: "fix: aged change", MergedAt: now.Add(-25 * time.Hour), IssueRefs: []string{"example/repo#1"}}},
		Checks:    []Check{{Name: "CI", Status: "completed", Conclusion: "success"}},
	}}
	service := New(Config{Enabled: true, MinMergedIssues: 5, MaxAge: 24 * time.Hour, RequireGreenCI: true}, backend)
	status, _ := service.Evaluate(context.Background(), now)
	if status.State != "release_pending" {
		t.Fatalf("Evaluate() state = %q, want release_pending", status.State)
	}
}

func TestTriggered(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 10, 20, 0, 0, 0, time.UTC)
	cfg := Config{MinMergedIssues: 3, MaxAge: 24 * time.Hour}

	tests := []struct {
		name   string
		status Status
		want   bool
	}{
		{name: "no merges and no age deadline", status: Status{}, want: false},
		{name: "below count without age deadline", status: Status{UnreleasedMerges: 2}, want: false},
		{name: "count at threshold", status: Status{UnreleasedMerges: 3}, want: true},
		{name: "count above threshold", status: Status{UnreleasedMerges: 7}, want: true},
		{name: "below count with future age deadline", status: Status{UnreleasedMerges: 1, NextTriggerAt: now.Add(time.Hour)}, want: false},
		{name: "below count with age deadline reached", status: Status{UnreleasedMerges: 1, NextTriggerAt: now}, want: true},
		{name: "below count with age deadline passed", status: Status{UnreleasedMerges: 1, NextTriggerAt: now.Add(-time.Minute)}, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := triggered(cfg, test.status, now); got != test.want {
				t.Fatalf("triggered() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestServiceExistingTagIsNotAFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 10, 20, 0, 0, 0, time.UTC)
	backend := &fakeBackend{
		tagErr: ErrTagExists,
		repo: Repository{
			Name:      "example/repo",
			HeadSHA:   "head",
			LatestTag: "v1.2.3",
			LatestSHA: "previous",
			Commits:   []Commit{{Message: "fix: retry status", MergedAt: now.Add(-time.Hour), IssueRefs: []string{"example/repo#1"}}},
			Checks:    []Check{{Name: "CI", Status: "completed", Conclusion: "success"}},
		},
	}
	service := New(Config{Enabled: true, MinMergedIssues: 1, MaxAge: 24 * time.Hour, RequireGreenCI: true}, backend)

	status, decision := service.Evaluate(context.Background(), now)
	if status.State != "release_pending" || status.PendingTag != "v1.2.4" {
		t.Fatalf("Evaluate() status = %#v, want release_pending v1.2.4", status)
	}
	if decision.Action != "tag_created" {
		t.Fatalf("Evaluate() decision = %#v, want tag_created", decision)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.failures) != 0 {
		t.Fatalf("failure issues = %#v, want none", backend.failures)
	}
}

func TestServiceFailedReleaseWorkflowFilesIssue(t *testing.T) {
	t.Parallel()

	backend := &fakeBackend{
		repo:          Repository{Name: "example/repo", HeadSHA: "head", LatestTag: "v1.3.0", LatestSHA: "head"},
		workflow:      WorkflowRun{ID: 9, URL: "https://example.test/runs/9", Status: "completed", Conclusion: "failure"},
		workflowFound: true,
	}
	service := New(Config{Enabled: true}, backend)
	status, _ := service.Evaluate(context.Background(), time.Now())
	if status.State != "failed" {
		t.Fatalf("Evaluate() state = %q, want failed", status.State)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.failures) != 1 || backend.failures[0].Fingerprint != "release_failed:example/repo:head" {
		t.Fatalf("failure issues = %#v", backend.failures)
	}
}

func TestChangelogGroupsCommitsAndIssueReferences(t *testing.T) {
	t.Parallel()

	got := Changelog("v1.3.0", []Commit{
		{Message: "fix: harden checks", IssueRefs: []string{"example/repo#2"}},
		{Message: "feat: add cadence", IssueRefs: []string{"example/repo#1"}},
		{Message: "chore: update docs"},
	})
	want := "v1.3.0\n\n## Features\n\n- feat: add cadence (example/repo#1)\n\n## Fixes\n\n- fix: harden checks (example/repo#2)\n\n## Other\n\n- chore: update docs"
	if got != want {
		t.Fatalf("Changelog() =\n%s\nwant:\n%s", got, want)
	}
}

type fakeBackend struct {
	mu            sync.Mutex
	repo          Repository
	workflow      WorkflowRun
	workflowFound bool
	inspectCalls  int
	tags          []Tag
	tagErr        error
	failures      []Failure
}

func (f *fakeBackend) Inspect(context.Context) (Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspectCalls++
	return f.repo, nil
}

func (f *fakeBackend) CreateTag(_ context.Context, tag Tag) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tagErr != nil {
		return f.tagErr
	}
	f.tags = append(f.tags, tag)
	f.repo.LatestTag = tag.Name
	f.repo.LatestSHA = tag.SHA
	f.repo.TaggedAt = time.Now()
	return nil
}

func (f *fakeBackend) ReleaseWorkflow(context.Context, string) (WorkflowRun, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workflow, f.workflowFound, nil
}

func (f *fakeBackend) RerunFailedChecks(context.Context, []Check) error {
	return nil
}

func (f *fakeBackend) EnsureFailureIssue(_ context.Context, failure Failure) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.failures {
		if existing.Fingerprint == failure.Fingerprint {
			return false, nil
		}
	}
	f.failures = append(f.failures, failure)
	return true, nil
}
