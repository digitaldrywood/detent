package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type landingHTTPClient func(*http.Request) (*http.Response, error)

func (f landingHTTPClient) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestLocalGitLandingReplacement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		cleanupFailure string
		supersededHead string
	}{
		{name: "closes the superseded head"},
		{name: "keeps a landing when listing superseded pulls fails", cleanupFailure: http.MethodGet},
		{name: "keeps a landing when commenting on a superseded pull fails", cleanupFailure: http.MethodPost},
		{name: "keeps a landing when closing a superseded pull fails", cleanupFailure: http.MethodPatch},
		{name: "retains a force pushed superseded PR", supersededHead: "moved"},
		{name: "retains a superseded PR without head evidence", supersededHead: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cleanupFailure := test.cleanupFailure
			f := newLandingFixture(t)
			repository := "https://github.com/example/repo"
			useFixtureGitHubRemote(t, f.source, f.remote, repository)
			f.advanceMain(t, "feature.txt", "parallel conflict\n")
			base := f.remoteMain(t)
			pulls := make(map[int]githubLandingPull)
			var landingInfo Info
			var requests int
			var comments []string
			wantComments := 1
			if cleanupFailure != "" {
				wantComments = 0
				if cleanupFailure != http.MethodGet {
					wantComments = 1
				}
			}
			client, err := github.NewClient(github.ClientConfig{
				TokenSource: github.StaticTokenSource(t.Name()), DisableConditionalRequests: true,
				HTTPClient: landingHTTPClient(func(req *http.Request) (*http.Response, error) {
					requests++
					status := http.StatusOK
					var response any
					var body map[string]string
					if req.Body != nil && req.URL.Path != "/graphql" {
						if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
					}
					switch {
					case req.Method == http.MethodGet && req.URL.Path == "/repos/example/repo/pulls":
						listed := []githubLandingPull{}
						for _, pull := range pulls {
							if req.URL.Query().Get("head") == "example:"+pull.Head.Ref || req.URL.Query().Get("state") == "open" && pull.State == "open" {
								listed = append(listed, pull)
							}
						}
						if req.URL.Query().Get("state") == "open" {
							kept, found := keptLanding(t.Context(), landingInfo.Path, pulls[18].Head.SHA, "refs/remotes/origin/main")
							if !found || kept.MergeSHA != pulls[18].Head.SHA || kept.BaseBefore != base || kept.Gate.Command != "git status --porcelain" {
								t.Fatalf("cleanup started without the kept landing: %#v, found=%t", kept, found)
							}
							if req.URL.Query().Get("base") != "main" {
								t.Fatal("cleanup did not scope its base")
							}
							if req.URL.Query().Get("page") == "1" {
								listed = make([]githubLandingPull, 100)
								for i := range listed {
									pull := pulls[17]
									pull.Number = i + 1
									switch i {
									case 0:
										pull.Head.Repo.FullName = "fork/repo"
									case 1:
										pull.Base.Repo.FullName = "other/repo"
									case 2:
										pull.Base.Ref = "develop"
									case 3:
										pull.Head.Ref = strings.TrimSuffix(pull.Head.Ref, f.head) + "invalid"
									case 4:
										pull.State = "closed"
									case 5:
										pull.Merged = true
									case 6:
										pull.MergedAt = "2026-10-05T15:31:37Z"
									case 17:
									default:
										pull.Head.Ref = "detent/landing/another-item/" + f.head
									}
									listed[i] = pull
								}
							} else if req.URL.Query().Get("page") != "2" {
								t.Fatalf("unexpected cleanup page %s", req.URL)
							}
						}
						response = listed
					case req.Method == http.MethodPost && req.URL.Path == "/repos/example/repo/pulls":
						pull := githubLandingPull{Number: len(pulls) + 17, State: "open", Body: body["body"]}
						_, pull.Head.Ref, _ = strings.Cut(body["head"], ":")
						pull.Head.SHA = strings.TrimSpace(runGit(t, f.remote, "rev-parse", "refs/heads/"+pull.Head.Ref))
						pull.Head.Repo.FullName, pull.Base.Repo.FullName = "example/repo", "example/repo"
						pull.Base.Ref, pull.Base.SHA = "main", base
						pulls[pull.Number], response = pull, pull
					case req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/merge"):
						if body["sha"] == f.head {
							status, response = http.StatusMethodNotAllowed, map[string]string{"message": "Pull Request has merge conflicts"}
						} else {
							pull := pulls[18]
							pull.State, pull.Merged = "closed", true
							pulls[18] = pull
							runGit(t, f.remote, "update-ref", "refs/heads/main", body["sha"])
							response = githubLandingMerge{Merged: true, SHA: body["sha"]}
						}
					case req.Method == http.MethodGet && req.URL.Path == "/repos/example/repo/pulls/17":
						response = pulls[17]
					case req.Method == http.MethodPost && req.URL.Path == "/repos/example/repo/issues/17/comments":
						comments = append(comments, body["body"])
						response = map[string]string{}
					case req.Method == http.MethodPatch && req.URL.Path == "/repos/example/repo/pulls/17":
						if body["state"] != "closed" || f.remoteMain(t) == base {
							t.Fatalf("cleanup before verified merge: %v", body)
						}
						pull := pulls[17]
						pull.State = "closed"
						pulls[17], response = pull, pull
					case req.URL.Path == "/graphql":
						pull := pulls[18]
						response = map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "example/repo", "pullRequest": map[string]any{"number": 18, "merged": true, "headRefOid": pull.Head.SHA, "headRefName": pull.Head.Ref, "baseRefName": "main", "headRepository": map[string]string{"nameWithOwner": "example/repo"}, "mergeCommit": map[string]string{"oid": pull.Head.SHA}}}}}
					default:
						t.Fatalf("unexpected landing request %s %s", req.Method, req.URL)
					}
					if req.Method == cleanupFailure && (req.URL.Query().Get("state") == "open" || strings.HasSuffix(req.URL.Path, "/comments") || req.Method == http.MethodPatch) {
						cleanupFailure = ""
						status, response = http.StatusBadGateway, map[string]string{"message": "cleanup unavailable"}
						if req.Method == http.MethodPatch {
							pull := pulls[17]
							pull.State = "open"
							pulls[17] = pull
						}
					}
					encoded, err := json.Marshal(response)
					if err != nil {
						t.Fatal(err)
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			opts := LandOptions{HeadSHA: f.head, Method: "squash", Repository: repository, Message: "Native Change Request", GitHubClient: client}
			issue := f.issue
			issue.Landing = &opts
			info, err := f.backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.backend.LandChangeViaGitHub(t.Context(), info, issue, opts)
			var refusal *LandRefusal
			if !errors.As(err, &refusal) || refusal.Kind != LandRefusalConflict || pulls[17].State != "open" {
				t.Fatalf("initial conflict: %v, pulls=%v", err, pulls)
			}
			tree := strings.TrimSpace(runGit(t, f.source, "rev-parse", f.head+"^{tree}"))
			opts.HeadSHA = strings.TrimSpace(runGit(t, f.source, "commit-tree", tree, "-p", base, "-m", "Resolved rebased head"))
			opts.ValidationCommand = "git status --porcelain"
			wantState := "closed"
			if test.supersededHead != "" {
				wantState, wantComments = "open", 0
				pull := pulls[17]
				if test.supersededHead == "moved" {
					pull.Head.SHA = opts.HeadSHA
					runGit(t, f.source, "push", "--force", "origin", opts.HeadSHA+":refs/heads/"+pull.Head.Ref)
				} else {
					pull.Head.SHA = ""
				}
				pulls[17] = pull
			}
			info, err = f.backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			landingInfo = info
			result, err := f.backend.LandChangeViaGitHub(t.Context(), info, issue, opts)
			if test.cleanupFailure != "" {
				wantState = "open"
				if !errors.Is(err, forgeavailability.ErrUnavailable) || errors.As(err, &refusal) || !pulls[18].Merged || result.MergeSHA != opts.HeadSHA {
					t.Fatalf("cleanup failure changed merged outcome: %#v, %v", result, err)
				}
				previousRequests := requests
				kept, retryErr := f.backend.LandChangeViaGitHub(t.Context(), info, issue, opts)
				if retryErr != nil || !reflect.DeepEqual(kept, result) || requests != previousRequests {
					t.Fatalf("second landing did not reuse the kept result: %#v, error=%v, requests=%d want=%d", kept, retryErr, requests, previousRequests)
				}
				err = retryErr
			}
			if err != nil || !pulls[18].Merged || pulls[17].State != wantState || len(comments) != wantComments || wantComments > 0 && !strings.Contains(comments[0], repository+"/pull/18") {
				t.Fatalf("replacement leaked conflicted pull: err=%v pulls=%v comments=%v", err, pulls, comments)
			}
		})
	}
}

func runPausedGitHubLanding(t *testing.T, fixture landingFixture, opts LandOptions, scenario string) (LandResult, error) {
	t.Helper()
	base := fixture.remoteMain(t)
	capture, err := CaptureChangeSource(t.Context(), fixture.info.Path, base, fixture.head)
	if err != nil {
		t.Fatal(err)
	}
	metadata := capture.Source
	otherIssue := Issue{Identifier: "DD-INDEPENDENT", Source: &ChangeSource{
		Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: fixture.head, Repository: opts.Repository, Source: &metadata}},
		Bundle:  capture.Bundle,
	}}
	other, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "independent"), SourceRoot: fixture.info.Path, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	if scenario == "bootstrap" {
		cache, err := toolcache.NewManaged(ctx, filepath.Join(t.TempDir(), "build-cache"), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var releaseCache func() error
		ctx, releaseCache, err = cache.Use(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := releaseCache(); err != nil {
				t.Error(err)
			}
		}()
		fixture.backend.hooks.AfterCreate = "test -f feature.txt"
		fixture.backend.hooks.BeforeRun = "test -f feature.txt"
	}
	paused, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	commands := 0
	process := validationProcessFixture{
		started:  func(procgroup.Identity) error { return nil },
		finished: func(gate.CommandResult, error) error { return nil },
	}
	ctx = WithValidationProcessOwner(ctx, func(ctx context.Context, _ Info, _ Issue, _ string) (ValidationProcess, error) {
		commands++
		pauseAt := 1
		if scenario == "refresh" {
			pauseAt = 2
		}
		if commands != pauseAt {
			return process, nil
		}
		close(paused)
		select {
		case <-resume:
			return process, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	authorityErr := errors.New("landing authority revoked")
	authorized := true
	opts.Authorize = func(context.Context) error {
		if !authorized {
			return authorityErr
		}
		return nil
	}
	if scenario == "failure" {
		opts.ValidationCommand = "exit 23"
	}
	var result LandResult
	var landingErr error
	go func() {
		defer close(done)
		result, landingErr = fixture.backend.LandChangeViaGitHub(ctx, fixture.info, fixture.issue, opts)
	}()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case <-paused:
	case <-done:
		t.Fatalf("landing did not reach validation: %v", landingErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	independent, err := other.Create(ctx, otherIssue)
	if err != nil {
		t.Fatalf("independent source import blocked behind paused validation: %v", err)
	}
	if got := strings.TrimSpace(runGit(t, independent.Path, "rev-parse", "HEAD")); got != fixture.head {
		t.Fatalf("imported head = %s, want %s", got, fixture.head)
	}
	release, err := fixture.backend.acquireSourceOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	common, err := gitCommonDir(ctx, other.sourceRoot)
	if err != nil {
		release()
		t.Fatal(err)
	}
	shared := sourceOperationLockFor(common)
	select {
	case <-shared.permit:
		shared.permit <- struct{}{}
		release()
		t.Fatal("independent workspace does not share the held source mutation lock")
	default:
	}
	mutationStarted, mutationDone := make(chan struct{}), make(chan error, 1)
	const mutationRef = "refs/heads/independent-mutation"
	go func() {
		close(mutationStarted)
		release, err := other.acquireSourceOperation(ctx)
		if err != nil {
			mutationDone <- err
			return
		}
		defer release()
		_, err = runGitAt(ctx, independent.Path, "update-ref", mutationRef, fixture.head)
		mutationDone <- err
	}()
	<-mutationStarted
	before, beforeErr := runGitAt(ctx, independent.Path, "for-each-ref", "--format=%(objectname)", mutationRef)
	release()
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	if beforeErr != nil || strings.TrimSpace(before) != "" {
		t.Fatalf("shared ref mutated while production lock was held: %s, %v", before, beforeErr)
	}
	if got := strings.TrimSpace(runGit(t, independent.Path, "rev-parse", mutationRef)); got != fixture.head {
		t.Fatalf("shared ref mutation did not finish after release: %s", got)
	}
	if err := os.WriteFile(filepath.Join(independent.Path, "unrelated.txt"), []byte("preserve independent edits\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preserved := preserveLandingOwner(t, independent.Path)
	switch scenario {
	case "base":
		fixture.advanceMain(t, "parallel.txt", "parallel\n")
	case "head":
		runGit(t, fixture.info.Path, "reset", "--hard", base)
	case "authority":
		authorized = false
	case "cancel":
		cancel()
	}
	close(resume)
	<-done
	preserved()
	release, err = other.acquireSourceOperation(t.Context())
	if err != nil {
		t.Fatalf("landing leaked the source lock: %v", err)
	}
	release()
	staging := filepath.Join(fixture.backend.root, "landing-"+fixture.info.Key)
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("landing staging survived cleanup: %v", err)
	}
	if listing := runGit(t, fixture.source, "worktree", "list", "--porcelain"); strings.Contains(listing, staging) || !strings.Contains(listing, independent.Path) {
		t.Fatalf("landing cleanup changed unrelated worktree registration: %s", listing)
	}
	var refusal *LandRefusal
	var validation *ValidationError
	switch scenario {
	case "base", "head":
		want := LandRefusalBaseMoved
		if scenario == "head" {
			want = LandRefusalHeadMoved
		}
		if !errors.As(landingErr, &refusal) || refusal.Kind != want {
			t.Fatalf("landing error = %v, want %s", landingErr, want)
		}
	case "authority":
		if !errors.Is(landingErr, authorityErr) {
			t.Fatalf("landing ignored revoked authority: %v", landingErr)
		}
	case "cancel":
		if !errors.Is(landingErr, context.Canceled) {
			t.Fatalf("landing ignored cancellation: %v", landingErr)
		}
	case "failure":
		if !errors.As(landingErr, &validation) || result.Gate.ExitCode != 23 {
			t.Fatalf("landing ignored failed validation: %+v, %v", result, landingErr)
		}
	}
	return result, landingErr
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
			useFixtureGitHubRemote(t, fixture.source, fixture.remote, repository)
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

func useFixtureGitHubRemote(t *testing.T, checkout, fixture, repository string) {
	t.Helper()
	script := `case "$3" in
		git-upload-pack\ *) exec git upload-pack "$1";;
		git-receive-pack\ *) exec git receive-pack "$1";;
		*) exit 1;;
	esac`
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	runGit(t, checkout, "config", "core.sshCommand", "sh -c "+quote(script)+" -- "+quote(fixture))
	runGit(t, checkout, "config", "ssh.variant", "simple")
	runGit(t, checkout, "remote", "set-url", "origin", "git@github.com:"+strings.TrimPrefix(repository, "https://github.com/")+".git")
}
