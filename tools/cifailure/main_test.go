package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/issueorigin"
)

type fakeCreator struct {
	drafts []intake.IssueDraft
	err    error
}

func (f *fakeCreator) CreateIntakeIssue(_ context.Context, draft intake.IssueDraft) (intake.Issue, error) {
	f.drafts = append(f.drafts, draft)
	return intake.Issue{}, f.err
}

func TestReport(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, input string
		storeErr    error
		wantCount   int
		wantErr     bool
	}{
		{"failure", `{"name":"Windows Core","conclusion":"failure","html_url":"https://example.test/job/1"}`, nil, 1, false},
		{"timeout", `{"name":"Windows Core","conclusion":"timed_out"}`, nil, 1, false},
		{"success", `{"name":"Windows Core","conclusion":"success"}`, nil, 0, false},
		{"skipped", `{"name":"Windows Core","conclusion":"skipped"}`, nil, 0, false},
		{"cancelled", `{"name":"Windows Core","conclusion":"cancelled"}`, nil, 0, false},
		{"fast job", `{"name":"Lint","conclusion":"failure"}`, nil, 0, false},
		{"malformed", `{`, nil, 0, true},
		{"report error continues", `{"name":"Windows Core","conclusion":"failure"} {"name":"GoReleaser Snapshot","conclusion":"failure"}`, errors.New("unavailable"), 2, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := &fakeCreator{err: tt.storeErr}
			err := report(t.Context(), strings.NewReader(tt.input), backend, "abc123")
			if (err != nil) != tt.wantErr || len(backend.drafts) != tt.wantCount {
				t.Fatalf("report = %v, %d drafts; want error %v, %d drafts", err, len(backend.drafts), tt.wantErr, tt.wantCount)
			}
			for _, draft := range backend.drafts {
				origin, ok := issueorigin.Parse(draft.Body)
				if !ok || origin.Kind != "doctor" || origin.Fingerprint == "" || !strings.Contains(draft.Body, "abc123") {
					t.Fatalf("invalid draft: %+v", draft)
				}
			}
		})
	}
}

func TestJobFingerprints(t *testing.T) {
	t.Parallel()
	names := []string{
		"Portability Verify (macos-latest)", "Portability Verify (windows-latest)",
		"Windows Core", "Installer Smoke (ubuntu-latest)", "Installer Smoke (windows-latest)",
		"GoReleaser Snapshot",
	}
	seen := map[string]bool{}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			backend := &fakeCreator{}
			for _, url := range []string{"first", "second"} {
				input := `{"name":"` + name + `","conclusion":"failure","html_url":"` + url + `"}`
				if err := report(t.Context(), strings.NewReader(input), backend, url); err != nil {
					t.Fatal(err)
				}
			}
			if len(backend.drafts) != 2 {
				t.Fatalf("got %d drafts", len(backend.drafts))
			}
			first, _ := issueorigin.Parse(backend.drafts[0].Body)
			second, _ := issueorigin.Parse(backend.drafts[1].Body)
			if first.Fingerprint != second.Fingerprint || seen[first.Fingerprint] {
				t.Fatal("fingerprint must be stable across runs and distinct across jobs")
			}
			seen[first.Fingerprint] = true
		})
	}
}

func TestRunEmptyJobs(t *testing.T) {
	t.Parallel()
	err := run(strings.NewReader(""), func(key string) string {
		switch key {
		case "GH_TOKEN":
			return "test-token"
		case "GITHUB_REPOSITORY":
			return "owner/repo"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}
