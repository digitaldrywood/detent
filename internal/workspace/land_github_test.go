package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector/github"
)

type landingHTTPClient func(*http.Request) (*http.Response, error)

func (f landingHTTPClient) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestLocalGitLandChangeViaGitHub(t *testing.T) {
	for _, test := range []struct {
		name          string
		method        string
		pullState     string
		reworked      bool
		failureMethod string
		status        int
		message       string
		rate          bool
		retryAfter    string
	}{
		{name: "merges the reviewed head", method: "merge"},
		{name: "uses the policy squash method", method: "squash"},
		{name: "uses the policy rebase method", method: "rebase"},
		{name: "reuses an open PR", method: "merge", pullState: "open"},
		{name: "records an already merged reviewed head", method: "merge", pullState: "merged"},
		{name: "ignores an older merged PR", method: "merge", pullState: "older"},
		{name: "publishes a reworked branch", method: "squash", reworked: true},
		{name: "authentication 403", method: "merge", failureMethod: "GET", status: 403, message: "Resource not accessible by integration"},
		{name: "protected merge 403", method: "merge", failureMethod: "PUT", status: 403, message: "Protected branch update failed"},
		{name: "required reviews", method: "merge", failureMethod: "PUT", status: 405, message: "Branch protection requires reviews"},
		{name: "required checks", method: "merge", failureMethod: "PUT", status: 405, message: "Required status checks have not passed"},
		{name: "read primary quota 403", method: "merge", failureMethod: "GET", status: 403, message: "API rate limit exceeded for user585100", rate: true},
		{name: "create primary quota 403", method: "merge", failureMethod: "POST", status: 403, message: "API rate limit exceeded", rate: true},
		{name: "merge primary quota 403", method: "merge", failureMethod: "PUT", status: 403, message: "API rate limit exceeded", rate: true},
		{name: "read secondary quota 429", method: "merge", failureMethod: "GET", status: 429, message: "secondary rate limit", rate: true, retryAfter: "120"},
		{name: "create secondary quota 429", method: "merge", failureMethod: "POST", status: 429, message: "secondary rate limit", rate: true, retryAfter: "120"},
		{name: "merge secondary quota 429", method: "merge", failureMethod: "PUT", status: 429, message: "secondary rate limit", rate: true, retryAfter: "120"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			fixture := newLandingFixture(t)
			base := fixture.remoteMain(t)
			repository := "https://github.com/example/repo"
			runGit(t, fixture.source, "config", "url.file://"+fixture.remote+".insteadOf", repository+".git")
			runGit(t, fixture.source, "remote", "set-url", "origin", repository+".git")
			if test.reworked {
				tree := strings.TrimSpace(runGit(t, fixture.source, "rev-parse", fixture.head+"^{tree}"))
				previous := strings.TrimSpace(runGit(t, fixture.source, "commit-tree", tree, "-p", base, "-m", "Previous attempt"))
				runGit(t, fixture.source, "push", "origin", previous+":refs/heads/"+fixture.info.Branch)
			}
			var methods []string
			createdPull := false
			reset := time.Now().Add(time.Hour).Truncate(time.Second)
			newClient := func(healthy bool) *github.Client {
				client, err := github.NewClient(github.ClientConfig{
					TokenSource:                github.StaticTokenSource(test.name + strconv.FormatInt(time.Now().UnixNano(), 10)),
					DisableConditionalRequests: true,
					HTTPClient: landingHTTPClient(func(req *http.Request) (*http.Response, error) {
						methods = append(methods, req.Method)
						status := http.StatusOK
						headers := make(http.Header)
						headers.Set("X-RateLimit-Limit", "5000")
						headers.Set("X-RateLimit-Remaining", "4990")
						headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
						pull := fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":"%s"},"base":{"ref":"main"}}`, fixture.head)
						var response string
						if !healthy && req.Method == test.failureMethod {
							status = test.status
							response = fmt.Sprintf(`{"message":%q}`, test.message)
							if test.rate && test.retryAfter == "" {
								headers.Set("X-RateLimit-Remaining", "0")
							}
							if test.retryAfter != "" {
								headers.Del("X-RateLimit-Reset")
								headers.Set("Retry-After", test.retryAfter)
							}
						} else {
							switch req.Method {
							case "GET":
								switch {
								case createdPull:
									response = "[" + pull + "]"
								case test.pullState == "open":
									response = "[" + pull + "]"
								case test.pullState == "merged":
									runGit(t, fixture.remote, "update-ref", "refs/heads/main", fixture.head)
									response = fmt.Sprintf(`[{"number":7,"state":"closed","merged":true,"merge_commit_sha":"%s","head":{"sha":"%s"},"base":{"ref":"main"}}]`, fixture.head, fixture.head)
								case test.pullState == "older":
									response = fmt.Sprintf(`[{"number":6,"state":"closed","merged":true,"head":{"sha":"%s"},"base":{"ref":"main"}}]`, base)
								default:
									response = "[]"
								}
							case "POST":
								createdPull = true
								response = pull
							case "PUT":
								var body map[string]string
								if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
									t.Fatal(err)
								}
								if body["sha"] != fixture.head || body["merge_method"] != test.method {
									t.Fatalf("merge body = %#v", body)
								}
								runGit(t, fixture.remote, "update-ref", "refs/heads/main", fixture.head)
								response = fmt.Sprintf(`{"merged":true,"sha":"%s"}`, fixture.head)
							}
						}
						return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(response))}, nil
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				return client
			}
			client := newClient(false)
			opts := LandOptions{HeadSHA: fixture.head, Method: test.method, Repository: repository, Message: "Native Change Request", GitHubClient: client}
			result, err := fixture.backend.LandChangeViaGitHub(context.Background(), fixture.info, fixture.issue, opts)
			if test.failureMethod != "" {
				var refusal *LandRefusal
				if test.rate {
					var status *github.StatusError
					if !errors.Is(err, github.ErrRateLimited) || errors.As(err, &refusal) || !errors.As(err, &status) || status.CredentialIdentity == "" || status.ObservedAt.IsZero() {
						t.Fatalf("quota error = %v", err)
					}
					if test.retryAfter == "" && !status.ResetAt.Equal(reset) || test.retryAfter != "" && (status.RetryAfter != 120*time.Second || !status.ResetAt.IsZero()) {
						t.Fatalf("quota evidence = %#v", status)
					}
					usage := client.FlushRESTRateLimitUsage()
					if !usage.RateLimited || usage.TotalRequests != int64(len(methods)) || len(usage.Budgets) == 0 {
						t.Fatalf("REST usage = %#v", usage)
					}
					if fixture.remoteMain(t) != base {
						t.Fatal("quota failure advanced base")
					}
					methods = nil
					opts.GitHubClient = newClient(true)
					result, err = fixture.backend.LandChangeViaGitHub(t.Context(), fixture.info, fixture.issue, opts)
				} else {
					if !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected || errors.Is(err, github.ErrRateLimited) {
						t.Fatalf("refusal = %v", err)
					}
					return
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.MergeSHA != fixture.head || result.BaseRef != "main" || result.Method != test.method || !result.AttemptBranchPushed || fixture.remoteMain(t) != fixture.head {
				t.Fatalf("landing = %#v", result)
			}
			wantCreate := test.pullState != "open" && test.pullState != "merged" && !(test.rate && test.failureMethod == "PUT")
			wantMerge := test.pullState != "merged"
			calls := strings.Join(methods, ",")
			if strings.Contains(calls, "POST") != wantCreate || strings.Contains(calls, "PUT") != wantMerge {
				t.Fatalf("operations = %v", methods)
			}
			if published := strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch)); published != fixture.head {
				t.Fatalf("published = %s", published)
			}
		})
	}
}
func TestGitHubLandingRepository(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		url, name, owner string
	}{
		{"https://github.com/example/repo", "example/repo", "example"},
		{"https://example.com/example/repo", "", ""},
		{"https://github.com/example/repo/extra", "", ""},
		{"https://github.com/%2e%2e/repo", "", ""},
		{"https://github.com/example/repo?token=secret", "", ""},
		{"https://user:secret@github.com/example/repo", "", ""},
	} {
		t.Run(test.url, func(t *testing.T) {
			name, owner, ok := githubLandingRepository(test.url)
			if name != test.name || owner != test.owner || ok != (test.name != "") {
				t.Fatalf("repository = %q, %q, %v", name, owner, ok)
			}
		})
	}
}
