package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type landingHTTPClient func(*http.Request) (*http.Response, error)

func (f landingHTTPClient) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestLocalGitLandChangeViaGitHub(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, test := range []struct {
		name           string
		method         string
		pullState      string
		reworked       bool
		failureMethod  string
		status         int
		message        string
		failureBody    string
		emptyBody      bool
		rate           bool
		retryAfter     string
		wantRefusal    string
		moved          bool
		external       bool
		isolated       bool
		pullError      string
		sourceConflict bool
		advanceOnMerge bool
		projection     string
		wantDeferred   bool
		refreshQuota   bool
		refreshStatus  int
		wantOutage     bool
		gitReadFailure string
		gitReadClass   string
	}{
		{name: "merges the reviewed head", method: "merge"},
		{name: "uses the policy squash method", method: "squash"},
		{name: "uses the policy rebase method", method: "rebase"},
		{name: "reuses an open PR", method: "merge", pullState: "open"},
		{name: "records an already merged reviewed head", method: "merge", pullState: "merged"},
		{name: "ignores an older merged PR", method: "merge", pullState: "older"},
		{name: "publishes a reworked branch despite a stale list head", method: "squash", reworked: true, pullState: "stale"},
		{name: "atomic merge rejects a genuinely moved head", method: "squash", reworked: true, pullState: "stale", moved: true, wantRefusal: LandRefusalHeadMoved},
		{name: "atomic 409 rejects the head without English text", method: "squash", pullState: "open", failureMethod: "PUT", status: 409, message: "La branche a été modifiée", wantRefusal: LandRefusalHeadMoved},
		{name: "atomic 409 rejects the head without a body", method: "squash", pullState: "open", failureMethod: "PUT", status: 409, emptyBody: true, wantRefusal: LandRefusalHeadMoved},
		{name: "atomic 409 rejects the head without JSON", method: "squash", pullState: "open", failureMethod: "PUT", status: 409, failureBody: "upstream refused the merge", wantRefusal: LandRefusalHeadMoved},
		{name: "atomic merge rejects a closed PR", method: "squash", pullState: "open", failureMethod: "PUT", status: 405, message: "Pull Request is closed", wantRefusal: LandRefusalHeadMoved},
		{name: "atomic merge rejects a PR that is not open", method: "squash", pullState: "open", failureMethod: "PUT", status: 405, message: "Pull Request is not open", wantRefusal: LandRefusalHeadMoved},
		{name: "authentication 403", method: "merge", failureMethod: "GET", status: 403, message: "Resource not accessible by integration"},
		{name: "authentication 401", method: "merge", failureMethod: "GET", status: 401, message: "Bad credentials"},
		{name: "protected merge 403", method: "merge", failureMethod: "PUT", status: 403, message: "Protected branch update failed"},
		{name: "required reviews", method: "merge", failureMethod: "PUT", status: 405, message: "Branch protection requires reviews"},
		{name: "required checks", method: "merge", failureMethod: "PUT", status: 405, message: "Required status checks have not passed"},
		{name: "strict head protection", method: "squash", pullState: "open", failureMethod: "PUT", status: 405, message: "Head branch is out of date. Review and try the merge again.", wantRefusal: LandRefusalProtected},
		{name: "merge queue protection", method: "squash", pullState: "open", failureMethod: "PUT", status: 405, message: "Pull request must be merged using the merge queue.", wantRefusal: LandRefusalProtected},
		{name: "base race wording in metadata retains protection", method: "squash", pullState: "open", failureMethod: "PUT", status: 405, failureBody: `{"message":"Head branch is out of date.","documentation_url":"Base branch was modified"}`, wantRefusal: LandRefusalProtected},
		{name: "malformed base race retains protection", method: "squash", pullState: "open", failureMethod: "PUT", status: 405, failureBody: `{"message":"Base branch was modified"`, wantRefusal: LandRefusalProtected},
		{name: "protected base 405", method: "merge", failureMethod: "PUT", status: 405, message: "Protected branch update failed"},
		{name: "unspecified merge refusal", method: "merge", failureMethod: "PUT", status: 405, message: "Method Not Allowed"},
		{name: "closed wording in metadata is not a head refusal", method: "merge", failureMethod: "PUT", status: 405, failureBody: `{"message":"Method Not Allowed","documentation_url":"Pull Request is closed"}`},
		{name: "conflict wording in metadata is not a conflict refusal", method: "merge", failureMethod: "PUT", status: 405, failureBody: `{"message":"Required status checks have not passed","errors":["Pull Request is not mergeable"]}`},
		{name: "malformed conflict text retains protection refusal", method: "merge", failureMethod: "PUT", status: 405, failureBody: `{"message":"Pull Request has merge conflicts"`},
		{name: "unknown clean source defers explicit merge conflict", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", wantDeferred: true},
		{name: "clean new rework head retains merge continuation", method: "squash", reworked: true, pullState: "stale", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", wantDeferred: true},
		{name: "conflict refresh quota retains capacity ownership", method: "merge", pullState: "open", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", refreshQuota: true, rate: true, retryAfter: "120"},
		{name: "real merge server failure retains outage ownership", method: "merge", failureMethod: "PUT", status: 503, message: "Service Unavailable", wantOutage: true},
		{name: "conflict refresh server failure retains outage ownership", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", refreshStatus: 503, wantOutage: true},
		{name: "conflict refresh protection retains refusal ownership", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", refreshStatus: 403, wantRefusal: LandRefusalProtected},
		{name: "Git refresh transport failure retains outage ownership", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", gitReadFailure: "Connection reset by peer", gitReadClass: forgeavailability.ClassTransport},
		{name: "Git refresh timeout retains outage ownership", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", gitReadFailure: "operation timed out", gitReadClass: forgeavailability.ClassTimeout},
		{name: "Git refresh authentication remains instance owned", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", gitReadFailure: "Authentication failed", gitReadClass: forgeavailability.ClassTransport},
		{name: "real source conflict reaches rework", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, wantRefusal: LandRefusalConflict},
		{name: "base advancing at refusal is inspected afresh", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, advanceOnMerge: true, wantRefusal: LandRefusalConflict},
		{name: "earlier head projection cannot prove a conflict", method: "squash", reworked: true, pullState: "stale", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, projection: "head", wantDeferred: true},
		{name: "stale base projection uses current conflicting base", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, projection: "base", wantRefusal: LandRefusalConflict},
		{name: "stale base projection with current clean base defers", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", projection: "base", wantDeferred: true},
		{name: "moved published head cannot prove reviewed conflict", method: "squash", reworked: true, pullState: "stale", moved: true, failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, wantDeferred: true},
		{name: "different PR branch cannot prove conflict", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, projection: "branch", wantDeferred: true},
		{name: "missing base evidence cannot prove conflict", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, projection: "missing", wantDeferred: true},
		{name: "unmergeable pull request", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request is not mergeable", wantDeferred: true},
		{name: "read refusal is not a merge conflict", method: "merge", failureMethod: "GET", status: 405, message: "Pull Request is not mergeable"},
		{name: "create refusal is not a head refusal", method: "merge", failureMethod: "POST", status: 405, message: "Pull Request is closed"},
		{name: "read primary quota 403", method: "merge", failureMethod: "GET", status: 403, message: "API rate limit exceeded for user585100", rate: true},
		{name: "create primary quota 403", method: "merge", failureMethod: "POST", status: 403, message: "API rate limit exceeded", rate: true},
		{name: "merge primary quota 403", method: "merge", failureMethod: "PUT", status: 403, message: "API rate limit exceeded", rate: true},
		{name: "primary quota wins over closed refusal text", method: "merge", failureMethod: "PUT", status: 403, message: "Pull Request is closed", rate: true},
		{name: "primary quota wins over strict head protection", method: "squash", failureMethod: "PUT", status: 403, message: "Head branch is out of date", rate: true},
		{name: "primary quota wins over base race", method: "squash", failureMethod: "PUT", status: 403, message: "Base branch was modified", rate: true},
		{name: "read secondary quota 429", method: "merge", failureMethod: "GET", status: 429, message: "secondary rate limit", rate: true, retryAfter: "120"},
		{name: "create secondary quota 429", method: "merge", failureMethod: "POST", status: 429, message: "secondary rate limit", rate: true, retryAfter: "120"},
		{name: "merge secondary quota 429", method: "merge", failureMethod: "PUT", status: 429, message: "secondary rate limit", rate: true, retryAfter: "120"},
		{name: "secondary quota wins over conflict refusal text", method: "merge", failureMethod: "PUT", status: 429, message: "Pull Request has merge conflicts", rate: true, retryAfter: "120"},
		{name: "reuses the explicit external PR", method: "merge", external: true},
		{name: "isolated checkout reuses the external source PR", method: "merge", external: true, isolated: true},
		{name: "isolated external source conflict", method: "squash", external: true, isolated: true, failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, wantRefusal: LandRefusalConflict},
		{name: "external moved head is never overwritten", method: "merge", external: true, pullError: "head", wantRefusal: LandRefusalHeadMoved},
		{name: "external branch must match", method: "merge", external: true, pullError: "branch", wantRefusal: LandRefusalHeadMoved},
		{name: "external repository must match", method: "merge", external: true, pullError: "repository", wantRefusal: LandRefusalProtected},
		{name: "external fork must match", method: "merge", external: true, pullError: "fork", wantRefusal: LandRefusalProtected},
		{name: "external base must match", method: "merge", external: true, pullError: "base", wantRefusal: LandRefusalProtected},
		{name: "external authentication denied", method: "merge", external: true, failureMethod: "GET", status: 401, message: "Bad credentials"},
		{name: "external primary quota 403", method: "merge", external: true, failureMethod: "GET", status: 403, message: "API rate limit exceeded", rate: true},
		{name: "external secondary quota 429", method: "merge", external: true, failureMethod: "GET", status: 429, message: "secondary rate limit", rate: true, retryAfter: "120"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.gitReadFailure != "" && runtime.GOOS == "windows" {
				t.Skip("Git command fault injection requires a POSIX shell")
			}
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			fixture := newLandingFixture(t)
			originalBase := fixture.remoteMain(t)
			if test.sourceConflict && !test.advanceOnMerge {
				fixture.advanceMain(t, "feature.txt", "base conflict\n")
			} else if test.projection == "base" {
				fixture.advanceMain(t, "parallel.txt", "parallel landing\n")
			}
			base := fixture.remoteMain(t)
			repository := "https://github.com/example/repo"
			runGit(t, fixture.source, "config", "url.file://"+fixture.remote+".insteadOf", repository+".git")
			runGit(t, fixture.source, "remote", "set-url", "origin", repository+".git")
			previous := base
			externalHead := fixture.head
			if test.external {
				if test.pullError == "head" {
					externalHead = base
				}
				runGit(t, fixture.source, "push", "origin", externalHead+":refs/heads/"+fixture.info.Branch)
			}
			if test.reworked {
				tree := strings.TrimSpace(runGit(t, fixture.source, "rev-parse", fixture.head+"^{tree}"))
				previous = strings.TrimSpace(runGit(t, fixture.source, "commit-tree", tree, "-p", base, "-m", "Previous attempt"))
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
						if req.URL.Path == "/graphql" && test.pullState == "merged" {
							response := fmt.Sprintf(`{"data":{"repository":{"nameWithOwner":"example/repo","pullRequest":{"number":7,"merged":true,"headRefOid":%q,"headRefName":%q,"baseRefName":"main","headRepository":{"nameWithOwner":"example/repo"},"mergeCommit":{"oid":%q}}}}}`, fixture.head, fixture.info.Branch, fixture.head)
							return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
						}
						methods = append(methods, req.Method)
						var body map[string]string
						if req.Method == http.MethodPut {
							if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
								t.Fatal(err)
							}
							if body["sha"] != fixture.head || body["merge_method"] != test.method {
								t.Fatalf("merge body = %#v", body)
							}
						}
						status := http.StatusOK
						headers := make(http.Header)
						headers.Set("X-RateLimit-Limit", "5000")
						headers.Set("X-RateLimit-Remaining", "4990")
						headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
						pull := fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":"%s"},"base":{"ref":"main"}}`, fixture.head)
						var response string
						if test.external {
							pullHead, headRef, baseRef, headRepo, baseRepo := externalHead, fixture.info.Branch, "main", "example/repo", "example/repo"
							switch test.pullError {
							case "branch":
								headRef = "another-branch"
							case "base":
								baseRef = "another-base"
							case "repository":
								baseRepo = "another/repo"
							case "fork":
								headRepo = "another/repo"
							}
							pull = fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":"%s","ref":"%s","repo":{"full_name":"%s"}},"base":{"sha":"%s","ref":"%s","repo":{"full_name":"%s"}}}`, pullHead, headRef, headRepo, base, baseRef, baseRepo)
						}
						if !healthy && req.Method == test.failureMethod {
							status = test.status
							if test.advanceOnMerge {
								fixture.advanceMain(t, "feature.txt", "base conflict\n")
								base = fixture.remoteMain(t)
							}
							response = fmt.Sprintf(`{"message":%q}`, test.message)
							if test.failureBody != "" || test.emptyBody {
								response = test.failureBody
							}
							if test.rate && test.retryAfter == "" && !test.refreshQuota {
								headers.Set("X-RateLimit-Remaining", "0")
							}
							if test.retryAfter != "" && !test.refreshQuota {
								headers.Del("X-RateLimit-Reset")
								headers.Set("Retry-After", test.retryAfter)
							}
						} else {
							switch req.Method {
							case "GET":
								published := strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch))
								if !test.external && req.URL.RawQuery != "" && published != fixture.head {
									t.Fatalf("list read preceded reviewed head publication: %s", published)
								}
								switch {
								case req.URL.Path == "/repos/example/repo/pulls/7" && !test.external:
									pullHead, pullBase, pullBranch := fixture.head, fixture.remoteMain(t), fixture.info.Branch
									switch test.projection {
									case "head":
										pullHead = previous
									case "base":
										pullBase = originalBase
									case "branch":
										pullBranch = "other"
									case "missing":
										pullBase = ""
									}
									response = fmt.Sprintf(`{"number":7,"state":"open","mergeable":null,"mergeable_state":"unknown","head":{"sha":"%s","ref":"%s","repo":{"full_name":"example/repo"}},"base":{"sha":"%s","ref":"main","repo":{"full_name":"example/repo"}}}`, pullHead, pullBranch, pullBase)
									if test.refreshQuota && !healthy {
										status = http.StatusTooManyRequests
										headers.Del("X-RateLimit-Reset")
										headers.Set("Retry-After", "120")
										response = `{"message":"secondary rate limit"}`
									}
									if test.refreshStatus != 0 && !healthy {
										status = test.refreshStatus
										response = `{"message":"refresh refused"}`
									}
									if test.gitReadFailure != "" && !healthy {
										realGit, err := exec.LookPath("git")
										if err != nil {
											t.Fatal(err)
										}
										wrapper := t.TempDir()
										script := "#!/bin/sh\n" +
											"if [ \"$1\" = \"-C\" ] && [ \"$3\" = \"fetch\" ]; then\n" +
											"printf '%s\\n' " + shellQuote("fatal: unable to access github.com: "+test.gitReadFailure) + " >&2\nexit 128\nfi\n" +
											"exec " + shellQuote(realGit) + " \"$@\"\n"
										if err := os.WriteFile(filepath.Join(wrapper, "git"), []byte(script), 0o700); err != nil {
											t.Fatal(err)
										}
										t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
									}
								case test.external:
									if req.URL.Path != "/repos/example/repo/pulls/7" || req.URL.RawQuery != "" {
										t.Fatalf("external PR lookup = %s", req.URL)
									}
									response = pull
								case createdPull:
									response = "[" + pull + "]"
								case test.pullState == "open":
									response = "[" + pull + "]"
								case test.pullState == "stale":
									response = "[" + strings.ReplaceAll(pull, fixture.head, previous) + "]"
									if test.moved {
										runGit(t, fixture.remote, "update-ref", "refs/heads/"+fixture.info.Branch, previous)
									}
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
								published := strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch))
								if published != body["sha"] {
									status = http.StatusConflict
									response = `{"message":"Head branch was modified. Review and try the merge again."}`
								} else {
									runGit(t, fixture.remote, "update-ref", "refs/heads/main", fixture.head)
									response = fmt.Sprintf(`{"merged":true,"sha":"%s"}`, fixture.head)
								}
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
			if test.external {
				opts.External = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: repository + "/pull/7"}
			}
			landingInfo, landingIssue := fixture.info, fixture.issue
			if test.isolated {
				var err error
				runGit(t, fixture.source, "push", "origin", fixture.head+":refs/pull/7/head")
				landingIssue.Landing = &opts
				landingInfo, err = fixture.backend.Create(t.Context(), landingIssue)
				if err != nil {
					t.Fatal(err)
				}
				if landingInfo.Path == fixture.info.Path || landingInfo.Branch == fixture.info.Branch || landingInfo.ReviewBranch != fixture.info.Branch {
					t.Fatalf("landing ownership = %#v", landingInfo)
				}
			}
			result, err := fixture.backend.LandChangeViaGitHub(context.Background(), landingInfo, landingIssue, opts)
			if test.gitReadFailure != "" {
				availability, ok := forgeavailability.As(err)
				if !ok || availability.Class != test.gitReadClass || availability.Scope.Operation != "git fetch" || !strings.Contains(err.Error(), test.message) || result.MergeSHA != "" || fixture.remoteMain(t) != base {
					t.Fatalf("Git transport failure lost its authentic owner: %#v, %v", result, err)
				}
				return
			}
			if test.wantOutage {
				availability, ok := forgeavailability.As(err)
				var status *github.StatusError
				if !ok || availability.Class != forgeavailability.ClassServer || !errors.As(err, &status) || status.StatusCode != 503 || result.MergeSHA != "" || fixture.remoteMain(t) != base {
					t.Fatalf("genuine outage lost its owner: %#v, %v", result, err)
				}
				return
			}
			if test.wantDeferred {
				var status *github.StatusError
				var refusal *LandRefusal
				if errors.Is(err, forgeavailability.ErrUnavailable) || !errors.As(err, &status) || status.StatusCode != 405 || !strings.Contains(status.Body, test.message) || !errors.As(err, &refusal) || refusal.Kind != LandRefusalBaseMoved || result.MergeSHA != "" || fixture.remoteMain(t) != base {
					t.Fatalf("unproven conflict = %#v, %v; base = %s", result, err, fixture.remoteMain(t))
				}
				if strings.Join(methods, ",") != "GET,PUT,GET" && strings.Join(methods, ",") != "GET,POST,PUT,GET" {
					t.Fatalf("conflict refresh sequence = %v", methods)
				}
				return
			}
			if test.wantRefusal != "" {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != test.wantRefusal || errors.Is(err, github.ErrRateLimited) || fixture.remoteMain(t) != base || result.MergeSHA != "" {
					t.Fatalf("merge refusal = %#v, %v; base = %s", result, err, fixture.remoteMain(t))
				}
				if !test.external && (!strings.Contains(strings.Join(methods, ","), "PUT") || !strings.Contains(refusal.Reason, "GitHub refused")) {
					t.Fatalf("refusal did not come from the atomic merge: %v, %v", err, methods)
				}
				if test.external {
					published := strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch))
					if published != externalHead {
						t.Fatalf("external branch was rewritten: %s", published)
					}
				}
				return
			}
			if test.failureMethod != "" {
				var refusal *LandRefusal
				if test.rate {
					var status *github.StatusError
					if !errors.Is(err, github.ErrRateLimited) || errors.As(err, &refusal) || !errors.As(err, &status) || status.CredentialIdentity == "" || status.ObservedAt.IsZero() {
						t.Fatalf("quota error = %v", err)
					}
					if test.refreshQuota && (errors.Is(err, forgeavailability.ErrUnavailable) || !strings.Contains(err.Error(), test.message)) {
						t.Fatalf("refresh quota lost precedence or original refusal: %v", err)
					}
					if test.retryAfter == "" && !status.ResetAt.Equal(reset) || test.retryAfter != "" && (status.RetryAfter != 120*time.Second || !status.ResetAt.IsZero()) {
						t.Fatalf("quota evidence = %#v", status)
					}
					usage := client.FlushRESTRateLimitUsage()
					if !usage.RateLimited || usage.TotalRequests != int64(len(methods)) || len(usage.Budgets) == 0 {
						t.Fatalf("REST usage = %#v", usage)
					}
					if fixture.remoteMain(t) != base || result.MergeSHA != "" {
						t.Fatal("quota failure advanced base")
					}
					methods = nil
					opts.GitHubClient = newClient(true)
					result, err = fixture.backend.LandChangeViaGitHub(t.Context(), fixture.info, fixture.issue, opts)
				} else {
					if !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected || errors.Is(err, github.ErrRateLimited) || fixture.remoteMain(t) != base || result.MergeSHA != "" {
						t.Fatalf("refusal = %v", err)
					}
					return
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.MergeSHA != fixture.head || result.BaseRef != "main" || result.BaseBefore != base || result.Method != test.method || result.AttemptBranchPushed != !test.external || fixture.remoteMain(t) != fixture.head {
				t.Fatalf("landing = %#v", result)
			}
			wantCreate := !test.external && test.pullState != "open" && test.pullState != "stale" && test.pullState != "merged" && !(test.rate && test.failureMethod == "PUT")
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

func TestGitHubLandingAPIEndpointOwnership(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, method, path string
	}{
		{"branch merge", "PUT", "repos/example/repo/branches/main/merge"},
		{"repository merge", "PUT", "repos/example/repo/merge"},
		{"read merge", "GET", "repos/example/repo/pulls/7/merge"},
		{"create merge", "POST", "repos/example/repo/pulls/7/merge"},
		{"zero pull number", "PUT", "repos/example/repo/pulls/0/merge"},
		{"negative pull number", "PUT", "repos/example/repo/pulls/-7/merge"},
		{"nonnumeric pull number", "PUT", "repos/example/repo/pulls/main/merge"},
		{"noncanonical pull number", "PUT", "repos/example/repo/pulls/007/merge"},
		{"missing owner", "PUT", "repos//repo/pulls/7/merge"},
		{"missing repository", "PUT", "repos/example//pulls/7/merge"},
		{"wrong resource", "PUT", "repos/example/repo/issues/7/merge"},
		{"extra path component", "PUT", "repos/example/repo/pulls/7/extra/merge"},
		{"query suffix", "PUT", "repos/example/repo/pulls/7/merge?operation=merge"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, response := range []struct {
				status int
				body   string
			}{
				{http.StatusConflict, `{"message":"Head branch was modified"}`},
				{http.StatusMethodNotAllowed, `{"message":"Base branch was modified. Review and try the merge again."}`},
				{http.StatusMethodNotAllowed, `{"message":"Head branch is out of date. Review and try the merge again."}`},
				{http.StatusMethodNotAllowed, `{"message":"Pull Request has merge conflicts"}`},
			} {
				client, err := github.NewClient(github.ClientConfig{
					TokenSource: github.StaticTokenSource(test.name),
					HTTPClient: landingHTTPClient(func(*http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: response.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response.body))}, nil
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				var result githubLandingMerge
				err = githubLandingAPI(t.Context(), client, &result, test.method, test.path)
				var refusal *LandRefusal
				var status *github.StatusError
				if response.status == http.StatusMethodNotAllowed {
					if !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected || errors.Is(err, connector.ErrPullRequestBaseOutOfDate) || errors.Is(err, forgeavailability.ErrUnavailable) {
						t.Fatalf("unrelated 405 supplied base-race authority: %v", err)
					}
				} else if err == nil || errors.As(err, &refusal) || !errors.As(err, &status) || status.StatusCode != http.StatusConflict || !errors.Is(err, github.ErrUnexpectedStatus) || result.Merged || result.SHA != "" {
					t.Fatalf("unrelated operation = %#v, %v", result, err)
				}
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

func TestLocalGitLandChangeViaGitHubAlreadyMerged(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, test := range []struct {
		name     string
		external bool
		invalid  string
	}{
		{name: "modern runner-created PR"},
		{name: "modern external PR", external: true},
		{name: "wrong repository", external: true, invalid: "repository"},
		{name: "wrong PR number", external: true, invalid: "number"},
		{name: "moved head", external: true, invalid: "head"},
		{name: "wrong branch", external: true, invalid: "branch"},
		{name: "wrong base", external: true, invalid: "base"},
		{name: "wrong head repository", external: true, invalid: "fork"},
		{name: "not merged", external: true, invalid: "merged"},
		{name: "missing merge commit", external: true, invalid: "missing"},
		{name: "merge commit outside target", external: true, invalid: "ancestry"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLandingFixture(t)
			base := fixture.remoteMain(t)
			repository := "https://github.com/example/repo"
			runGit(t, fixture.source, "config", "url.file://"+fixture.remote+".insteadOf", repository+".git")
			runGit(t, fixture.source, "remote", "set-url", "origin", repository+".git")
			runGit(t, fixture.source, "push", "origin", fixture.head+":refs/heads/"+fixture.info.Branch)
			tree := strings.TrimSpace(runGit(t, fixture.source, "rev-parse", fixture.head+"^{tree}"))
			merge := strings.TrimSpace(runGit(t, fixture.source, "commit-tree", tree, "-p", base, "-m", "Actual squashed landing"))
			runGit(t, fixture.source, "push", "origin", merge+":refs/heads/main")
			pull := map[string]any{"number": 7, "merged": true, "headRefOid": fixture.head, "headRefName": fixture.info.Branch, "baseRefName": "main", "headRepository": map[string]any{"nameWithOwner": "example/repo"}, "mergeCommit": map[string]any{"oid": merge}}
			projectedRepository := "example/repo"
			switch test.invalid {
			case "repository":
				projectedRepository = "other/repo"
			case "number":
				pull["number"] = 8
			case "head":
				pull["headRefOid"] = base
			case "branch":
				pull["headRefName"] = "other"
			case "base":
				pull["baseRefName"] = "other"
			case "fork":
				pull["headRepository"] = map[string]any{"nameWithOwner": "other/repo"}
			case "merged":
				pull["merged"] = false
			case "missing":
				pull["mergeCommit"] = nil
			case "ancestry":
				pull["mergeCommit"] = map[string]any{"oid": fixture.head}
			}
			graphqlReads, restMutations := 0, 0
			client, err := github.NewClient(github.ClientConfig{TokenSource: github.StaticTokenSource(test.name + strconv.FormatInt(time.Now().UnixNano(), 10)), DisableConditionalRequests: true, HTTPClient: landingHTTPClient(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/graphql" {
					if req.Method != http.MethodPost {
						t.Fatalf("GraphQL method=%s", req.Method)
					}
					var body struct {
						Query     string         `json:"query"`
						Variables map[string]any `json:"variables"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(body.Query, "mergeCommit{oid}") || body.Variables["owner"] != "example" || body.Variables["name"] != "repo" || body.Variables["number"] != float64(7) {
						t.Fatalf("wrong commit projection=%+v", body)
					}
					graphqlReads++
					response, err := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": projectedRepository, "pullRequest": pull}}})
					if err != nil {
						t.Fatal(err)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(response)))}, nil
				}
				if req.Method != http.MethodGet {
					restMutations++
					t.Fatalf("already merged PR mutated: %s %s", req.Method, req.URL)
				}
				if req.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
					t.Fatalf("API version=%s", req.Header.Get("X-GitHub-Api-Version"))
				}
				response := fmt.Sprintf(`{"number":7,"state":"closed","merged":true,"merged_at":"2026-10-02T16:00:00Z","head":{"sha":%q,"ref":%q,"repo":{"full_name":"example/repo"}},"base":{"sha":%q,"ref":"main","repo":{"full_name":"example/repo"}}}`, fixture.head, fixture.info.Branch, merge)
				if req.URL.Path == "/repos/example/repo/pulls" {
					response = "[" + response + "]"
				} else if req.URL.Path != "/repos/example/repo/pulls/7" {
					t.Fatalf("unexpected read=%s", req.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			opts := LandOptions{HeadSHA: fixture.head, Method: "squash", Repository: repository, GitHubClient: client}
			if test.external {
				opts.External = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: repository + "/pull/7"}
			}
			result, err := fixture.backend.LandChangeViaGitHub(t.Context(), fixture.info, fixture.issue, opts)
			if graphqlReads != 1 || restMutations != 0 {
				t.Fatalf("reads=%d mutations=%d", graphqlReads, restMutations)
			}
			if test.invalid != "" {
				if err == nil || result.MergeSHA != "" {
					t.Fatalf("invalid source landed=%+v error=%v", result, err)
				}
			} else if err != nil || result.MergeSHA != merge || result.BaseRef != "main" {
				t.Fatalf("genuine landing=%+v error=%v", result, err)
			}
			if fixture.remoteMain(t) != merge {
				t.Fatal("already landed target changed")
			}
		})
	}
}
