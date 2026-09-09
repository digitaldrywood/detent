package release

import (
	"context"
	"testing"
	"time"
)

func TestReleaseRejectsIncompleteEvidence(t *testing.T) {
	t.Parallel()
	for _, conclusion := range []string{"skipped", "neutral", "cancelled", "failure", ""} {
		t.Run(conclusion, func(t *testing.T) {
			t.Parallel()
			backend := &fakeBackend{repo: Repository{
				Name: "example/repository", HeadSHA: "candidate", LatestTag: "v1.0.0",
				Commits: []Commit{{SHA: "candidate", Message: "fix: example", IssueRefs: []string{"#1"}}},
				Checks:  []Check{{Name: "required", Status: "completed", Conclusion: conclusion}},
			}}
			service := New(Config{Enabled: true, MinMergedIssues: 1}, backend)
			service.Evaluate(context.Background(), time.Now())
			if len(backend.tags) != 0 {
				t.Fatalf("created release tag for conclusion %q", conclusion)
			}
		})
	}
}
