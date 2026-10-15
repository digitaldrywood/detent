package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

type landingChecksReader func(context.Context, string, string, any, any) error

func (f landingChecksReader) REST(ctx context.Context, method, path string, body, result any) error {
	return f(ctx, method, path, body, result)
}

func TestReadLandingChecks(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	readFailure := errors.New("CI read unavailable")
	for _, test := range []struct {
		name      string
		checks    bool
		state     string
		wantState string
		failRead  bool
	}{
		{name: "commit status after first page", state: "success", wantState: "success"},
		{name: "pending status after first page", state: "pending", wantState: "pending"},
		{name: "failed status after first page", state: "failure", wantState: "failure"},
		{name: "check run after first page", checks: true, state: "success", wantState: "success"},
		{name: "pending check run after first page", checks: true, state: "in_progress", wantState: "pending"},
		{name: "failed check run after first page", checks: true, state: "failure", wantState: "failure"},
		{name: "no matching result after full page", wantState: "pending"},
		{name: "failed check run page is not green", checks: true, failRead: true},
		{name: "failed status page is not green", failRead: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pages := map[string]int{}
			client := landingChecksReader(func(_ context.Context, method, path string, body, result any) error {
				u, err := url.Parse(path)
				if err != nil || method != http.MethodGet || body != nil || !strings.HasPrefix(u.Path, "repos/example/repo/commits/"+head+"/") || u.Query().Get("per_page") != "100" {
					t.Fatalf("read lost exact head or pagination: %s %s", method, path)
				}
				firstPage := u.Query().Get("page") == "1"
				pages[u.Path]++
				if strings.HasSuffix(u.Path, "/check-runs") {
					r := result.(*restCheckRuns)
					if firstPage {
						r.CheckRuns = make([]restCheckRun, 100)
					} else if test.checks {
						if test.failRead {
							return readFailure
						}
						status, conclusion := "completed", test.state
						if conclusion == "in_progress" {
							status, conclusion = "in_progress", ""
						}
						r.CheckRuns = []restCheckRun{{Name: "Full CI", Status: status, Conclusion: conclusion}}
					}
				} else if strings.HasSuffix(u.Path, "/statuses") {
					r := result.(*[]restCommitStatus)
					if firstPage {
						*r = make([]restCommitStatus, 100)
					} else if !test.checks {
						if test.failRead {
							return readFailure
						}
						if test.state != "" {
							*r = []restCommitStatus{{Context: "Full CI", State: test.state}}
						}
					}
				} else {
					t.Fatalf("unexpected read %s", path)
				}
				return nil
			})
			receipt, err := ReadLandingChecks(t.Context(), client, "example/repo", head, []string{" Full CI ", "Full CI"})
			if test.failRead {
				if !errors.Is(err, readFailure) || receipt != nil {
					t.Fatalf("failed CI read became proof: %+v, %v", receipt, err)
				}
				return
			}
			if err != nil || receipt.State != test.wantState || receipt.HeadSHA != head || !slices.Equal(receipt.RequiredChecks, []string{"Full CI"}) || pages["repos/example/repo/commits/"+head+"/statuses"] != 2 || pages["repos/example/repo/commits/"+head+"/check-runs"] != 2 {
				t.Fatalf("paginated CI lost contract: %+v, %v, %v", receipt, err, pages)
			}
			if test.state == "" && !slices.Equal(receipt.MissingChecks, []string{"Full CI"}) {
				t.Fatalf("missing context has no trigger evidence: %+v", receipt)
			}
		})
	}
}
