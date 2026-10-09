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
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type landingHTTPClient func(*http.Request) (*http.Response, error)

func (f landingHTTPClient) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestLocalGitLandingReplacement(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
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
			cleanupFailure := test.cleanupFailure
			f := newLandingFixture(t)
			repository := "https://github.com/example/repo"
			runGit(t, f.source, "config", "url.file://"+f.remote+".insteadOf", repository+".git")
			runGit(t, f.source, "remote", "set-url", "origin", repository+".git")
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

func TestLocalGitLandChangeViaGitHub(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	if runtime.GOOS == "windows" {
		t.Skip("landing gate fixtures require a POSIX shell")
	}

	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, test := range []struct {
		name                string
		method              string
		pullState           string
		reworked            bool
		failureMethod       string
		status              int
		message             string
		failureBody         string
		emptyBody           bool
		rate                bool
		retryAfter          string
		wantRefusal         string
		moved               bool
		external            bool
		isolated            bool
		pullError           string
		sourceConflict      bool
		advanceOnMerge      bool
		projection          string
		wantDeferred        bool
		wantRetry           bool
		retrySuccess        bool
		retryGateFailure    bool
		retryHeadMoved      bool
		refreshQuota        bool
		refreshStatus       int
		wantOutage          bool
		gitReadFailure      string
		gitReadClass        string
		sourceIssues        bool
		existingBody        string
		wantPatch           bool
		gateFailure         bool
		preparation         bool
		preparationFailure  string
		retryPreparation    bool
		combinedGateFailure bool
		baseMovesDuringGate bool
		advanceParallel     bool
		ownedHeadRetry      bool
		repeatRun           bool
		retryStatus         int
		retryMoved          bool
		baseMovesDuringRead bool
		rolling             bool
		prepareOnly         bool
		stagedWork          bool
		lostCreate          bool
		authorityFail       int
		wrongPull           string
		pushRefused         bool
		targetBranch        string
		ambiguousEffect     bool
		wrongRemote         string
		duplicatePull       bool
		remoteNewer         bool
		remoteOnlyHead      bool
		preservePublication bool
	}{
		{name: "prepare refuses a different selected remote", prepareOnly: true, wrongRemote: "selected", method: "squash", wantRefusal: LandRefusalProtected},
		{name: "prepare refuses a different push repository", prepareOnly: true, wrongRemote: "pushurl", method: "squash", wantRefusal: LandRefusalProtected},
		{name: "prepare refuses duplicate matching PRs", prepareOnly: true, duplicatePull: true, preservePublication: true, method: "squash", wantRefusal: LandRefusalHeadMoved},
		{name: "landing refuses newer published head", remoteNewer: true, preservePublication: true, pullState: "open", method: "squash", wantRefusal: LandRefusalHeadMoved},
		{name: "prepare refuses newer remote-only published head", prepareOnly: true, remoteNewer: true, remoteOnlyHead: true, preservePublication: true, pullState: "open", method: "squash", wantRefusal: LandRefusalHeadMoved},
		{name: "prepare refuses newer published head", prepareOnly: true, remoteNewer: true, preservePublication: true, pullState: "open", method: "squash", wantRefusal: LandRefusalHeadMoved},
		{name: "prepare legitimate forward Rework", prepareOnly: true, reworked: true, pullState: "stale", method: "squash"},
		{name: "prepare existing committed work under human hold", prepareOnly: true, method: "squash"},
		{name: "prepare existing staged work under human hold", prepareOnly: true, stagedWork: true, method: "squash"},
		{name: "prepare uses the configured target branch", prepareOnly: true, targetBranch: "develop", method: "squash"},
		{name: "prepare adopts PR after lost create response", prepareOnly: true, lostCreate: true, method: "squash"},
		{name: "prepare refuses wrong PR head", prepareOnly: true, pullState: "open", wrongPull: "head", method: "squash", wantRefusal: LandRefusalHeadMoved},
		{name: "prepare refuses wrong PR repository", prepareOnly: true, pullState: "open", wrongPull: "repository", method: "squash", wantRefusal: LandRefusalHeadMoved},
		{name: "prepare refuses wrong PR target", prepareOnly: true, pullState: "open", wrongPull: "target", preservePublication: true, method: "squash", wantRefusal: LandRefusalHeadMoved},
		{name: "prepare preserves source on push refusal", prepareOnly: true, pushRefused: true, method: "squash", wantRefusal: LandRefusalProtected},
		{name: "prepare refuses lost authority before preparation", prepareOnly: true, authorityFail: 1, method: "squash"},
		{name: "prepare refuses lost authority before push", prepareOnly: true, authorityFail: 2, method: "squash"},
		{name: "prepare refuses lost authority before create", prepareOnly: true, authorityFail: 3, method: "squash"},
		{name: "prepare refuses lost authority before recording", prepareOnly: true, authorityFail: 4, method: "squash"},
		{name: "prepare refuses ambiguous external effect before push", prepareOnly: true, ambiguousEffect: true, authorityFail: 2, method: "squash"},
		{name: "prepare preserves source on authentication refusal", prepareOnly: true, method: "squash", failureMethod: "POST", status: 403, message: "Resource not accessible by integration", wantRefusal: LandRefusalProtected},
		{name: "rolling mode skips a failing gate", method: "squash", rolling: true},
		{name: "creates the exact source closing payload", method: "squash", sourceIssues: true},
		{name: "reuse preserves human delivery attribution", method: "squash", pullState: "open", sourceIssues: true, existingBody: "Human attribution\n\nCloses example/repo#44", wantPatch: true},
		{name: "reuse retains source lines without duplication", method: "squash", pullState: "open", sourceIssues: true, existingBody: "Human attribution\n\nCloses digitaldrywood/detent#3410"},
		{name: "external PR retains human attribution", method: "squash", external: true, sourceIssues: true, existingBody: "Human attribution", wantPatch: true},
		{name: "merged PR attribution is historical", method: "squash", pullState: "merged", sourceIssues: true, existingBody: "Historical attribution"},
		{name: "source attribution refuses a stale PR head", method: "squash", pullState: "stale", sourceIssues: true, existingBody: "Human attribution", wantRefusal: LandRefusalHeadMoved},
		{name: "merges the reviewed head", method: "merge"},
		{name: "red gate never publishes the source or calls the forge", method: "squash", gateFailure: true, preparation: true},
		{name: "gate rejects only the combined tree before delivery", method: "squash", projection: "base", combinedGateFailure: true},
		{name: "base advance during validation refuses merge", method: "squash", baseMovesDuringGate: true},
		{name: "uses the policy squash method", method: "squash"},
		{name: "uses the policy rebase method", method: "rebase"},
		{name: "reuses an open PR", method: "merge", pullState: "open"},
		{name: "records an already merged reviewed head", method: "merge", pullState: "merged"},
		{name: "ignores an older merged PR", method: "merge", pullState: "older"},
		{name: "publishes a reworked branch despite a stale list head", method: "squash", reworked: true, pullState: "stale"},
		{name: "atomic merge rejects a genuinely moved head", method: "squash", reworked: true, pullState: "stale", moved: true, wantRefusal: LandRefusalHeadMoved},
		{name: "atomic 409 rejects the head without English text", method: "squash", pullState: "open", failureMethod: "PUT", status: 409, message: "La branche a été modifiée", projection: "head", moved: true, wantRefusal: LandRefusalHeadMoved},
		{name: "atomic 409 rejects the head without a body", method: "squash", pullState: "open", failureMethod: "PUT", status: 409, emptyBody: true, projection: "head", moved: true, wantRefusal: LandRefusalHeadMoved},
		{name: "atomic 409 rejects the head without JSON", method: "squash", pullState: "open", failureMethod: "PUT", status: 409, failureBody: "upstream refused the merge", projection: "head", moved: true, wantRefusal: LandRefusalHeadMoved},
		{name: "base race retains item continuation", method: "squash", pullState: "open", failureMethod: "PUT", status: 405, message: "Base branch was modified. Review and try the merge again.", wantRefusal: LandRefusalBaseMoved},
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
		{name: "missing conflict projection retains verification failure", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", refreshStatus: 404},
		{name: "Git refresh transport failure retains outage ownership", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", gitReadFailure: "Connection reset by peer", gitReadClass: forgeavailability.ClassTransport},
		{name: "Git refresh timeout retains outage ownership", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", gitReadFailure: "operation timed out", gitReadClass: forgeavailability.ClassTimeout},
		{name: "Git refresh authentication remains instance owned", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", gitReadFailure: "Authentication failed", gitReadClass: forgeavailability.ClassTransport},
		{name: "real source conflict reaches rework", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, wantRefusal: LandRefusalConflict},
		{name: "base advancing at refusal is inspected afresh", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, advanceOnMerge: true, wantRefusal: LandRefusalConflict},
		{name: "earlier head projection cannot prove a conflict", method: "squash", reworked: true, pullState: "stale", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, projection: "head", wantDeferred: true},
		{name: "stale base projection uses current conflicting base", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, projection: "base", wantRefusal: LandRefusalConflict},
		{name: "staging preparation supplies ignored gate prerequisites", method: "squash", external: true, preparation: true},
		{name: "after_create failure cleans staging before publication", method: "squash", preparation: true, preparationFailure: "after_create"},
		{name: "before_run failure cleans staging before publication", method: "squash", preparation: true, preparationFailure: "before_run"},
		{name: "refresh after_create failure preserves reviewed and published source", method: "squash", preparation: true, preparationFailure: "after_create", retryPreparation: true, failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", advanceParallel: true},
		{name: "refresh before_run failure preserves reviewed and published source", method: "squash", preparation: true, preparationFailure: "before_run", retryPreparation: true, failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", advanceParallel: true},
		{name: "clean conflict retry preserves merge delivery", method: "merge", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", advanceParallel: true, retrySuccess: true, wantRetry: true},
		{name: "clean conflict retry preserves linear rebase delivery", method: "rebase", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", advanceParallel: true, retrySuccess: true, wantRetry: true},
		{name: "conflict rebases cleanly and lands once", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", advanceParallel: true, retrySuccess: true, wantRetry: true},
		{name: "isolated external conflict rebases cleanly", preparation: true, method: "squash", external: true, isolated: true, failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", advanceParallel: true, retrySuccess: true, wantRetry: true},
		{name: "retry refuses a branch moved after source verification", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", advanceParallel: true, retryHeadMoved: true},
		{name: "rebased landing reruns the gate before retry", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", advanceParallel: true, retryGateFailure: true},
		{name: "repeated 405 after a proven base move waits without further rewrites", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", projection: "base", advanceParallel: true, wantDeferred: true, wantRetry: true},
		{name: "moved published head cannot prove reviewed conflict", method: "squash", reworked: true, pullState: "stale", moved: true, failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, wantDeferred: true},
		{name: "different PR branch cannot prove conflict", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, projection: "branch", wantDeferred: true},
		{name: "missing base evidence cannot prove conflict", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request has merge conflicts", sourceConflict: true, projection: "missing", wantDeferred: true},
		{name: "unmergeable pull request with unchanged advanced base waits", method: "squash", projection: "base", repeatRun: true, failureMethod: "PUT", status: 405, message: "Pull Request is not mergeable", wantDeferred: true},
		{name: "409 retries the head published by an earlier landing run", method: "squash", pullState: "open", failureMethod: "PUT", status: 409, message: "Head branch was modified. Review and try the merge again.", repeatRun: true, ownedHeadRetry: true, retrySuccess: true},
		{name: "409 retries the head restored over a prior landing rewrite", method: "squash", reworked: true, pullState: "stale", failureMethod: "PUT", status: 409, message: "Head branch was modified. Review and try the merge again.", ownedHeadRetry: true, retrySuccess: true},
		{name: "repeated owned head 409 waits after one retry", method: "squash", pullState: "open", failureMethod: "PUT", status: 409, message: "Head branch was modified", ownedHeadRetry: true, wantDeferred: true},
		{name: "409 rejects a moved remote despite matching PR projection", method: "squash", reworked: true, pullState: "stale", moved: true, failureMethod: "PUT", status: 409, message: "Head branch was modified", wantRefusal: LandRefusalHeadMoved},
		{name: "stale 409 projection waits while the published head is unchanged", method: "squash", pullState: "open", failureMethod: "PUT", status: 409, message: "Head branch was modified", projection: "head", wantDeferred: true},
		{name: "409 after Detents refresh waits against its pushed head", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request is not mergeable", advanceParallel: true, wantRetry: true, retryStatus: 409, wantDeferred: true},
		{name: "409 after refresh refuses another actors head", method: "squash", failureMethod: "PUT", status: 405, message: "Pull Request is not mergeable", advanceParallel: true, wantRetry: true, retryStatus: 409, retryMoved: true, wantRefusal: LandRefusalHeadMoved},
		{name: "405 after owned head retry waits without rewriting", method: "squash", pullState: "open", failureMethod: "PUT", status: 409, message: "Pull Request is not mergeable", ownedHeadRetry: true, retryStatus: 405, wantDeferred: true},
		{name: "base race during 409 verification retains landing wait", method: "squash", pullState: "open", failureMethod: "PUT", status: 409, message: "Head branch was modified", baseMovesDuringRead: true},
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
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			fixture := newLandingFixture(t)
			originalBase := fixture.remoteMain(t)
			if test.sourceConflict && !test.advanceOnMerge {
				fixture.advanceMain(t, "feature.txt", "base conflict\n")
			} else if test.projection == "base" && !test.advanceParallel {
				fixture.advanceMain(t, "parallel.txt", "parallel landing\n")
			}
			base := fixture.remoteMain(t)
			target := "main"
			if test.targetBranch != "" {
				target = test.targetBranch
				runGit(t, fixture.remote, "update-ref", "refs/heads/"+target, base)
			}
			repository := "https://github.com/example/repo"
			runGit(t, fixture.source, "config", "url.file://"+fixture.remote+".insteadOf", repository+".git")
			runGit(t, fixture.source, "remote", "set-url", "origin", repository+".git")
			previous := base
			externalHead := fixture.head
			if test.pullState == "merged" {
				runGit(t, fixture.source, "push", "origin", fixture.head+":refs/heads/"+fixture.info.Branch)
			}
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
			if test.preservePublication {
				if test.remoteNewer {
					tree := strings.TrimSpace(runGit(t, fixture.source, "rev-parse", fixture.head+"^{tree}"))
					if test.remoteOnlyHead {
						runGit(t, fixture.source, "push", "origin", fixture.head+":refs/heads/"+fixture.info.Branch)
						previous = strings.TrimSpace(runGit(t, fixture.remote, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", tree, "-p", fixture.head, "-m", "Newer remote work"))
					} else {
						previous = strings.TrimSpace(runGit(t, fixture.source, "commit-tree", tree, "-p", fixture.head, "-m", "Newer published work"))
					}
				}
				if test.remoteOnlyHead {
					runGit(t, fixture.remote, "update-ref", "refs/heads/"+fixture.info.Branch, previous)
				} else {
					runGit(t, fixture.source, "push", "origin", previous+":refs/heads/"+fixture.info.Branch)
				}
			}
			runGit(t, fixture.remote, "config", "core.logAllRefUpdates", "true")
			preservedBranch := runGit(t, fixture.remote, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"+fixture.info.Branch)
			preservedLog := ""
			if test.preservePublication {
				preservedLog = runGit(t, fixture.remote, "reflog", "show", "--format=%H %gs", "refs/heads/"+fixture.info.Branch)
			}
			previousRun := test.repeatRun
			var methods []string
			var createCalls int
			createResponseLost := false
			mergeCalls := 0
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
						if strings.Contains(req.URL.Path, "/issues") {
							t.Fatalf("landing polled source issues: %s", req.URL)
						}
						if req.Method == http.MethodPut || req.Method == http.MethodPost || req.Method == http.MethodPatch {
							if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
								t.Fatal(err)
							}
							if req.Method == http.MethodPut {
								mergeCalls++
							}
							if req.Method == http.MethodPut && (mergeCalls == 1 && body["sha"] != fixture.head || body["merge_method"] != test.method) {
								t.Fatalf("merge body = %#v", body)
							}
						}
						status := http.StatusOK
						headers := make(http.Header)
						headers.Set("X-RateLimit-Limit", "5000")
						headers.Set("X-RateLimit-Remaining", "4990")
						headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
						pull := fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":"%s","ref":%q,"repo":{"full_name":"example/repo"}},"base":{"ref":"main","repo":{"full_name":"example/repo"}}}`, fixture.head, fixture.info.Branch)
						if test.targetBranch != "" {
							pull = strings.Replace(pull, `"ref":"main"`, `"ref":`+strconv.Quote(target), 1)
						}
						var response string
						if test.external {
							pullHead, headRef, baseRef, headRepo, baseRepo := externalHead, fixture.info.Branch, "main", "example/repo", "example/repo"
							if mergeCalls > 1 {
								pullHead = strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+headRef))
							}
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
						pull = strings.Replace(pull, `"number":7,`, fmt.Sprintf(`"number":7,"body":%q,`, test.existingBody), 1)
						switch test.wrongPull {
						case "head":
							pull = strings.ReplaceAll(pull, fixture.head, originalBase)
						case "repository":
							pull = strings.ReplaceAll(pull, "example/repo", "other/repo")
						case "target":
							pull = strings.ReplaceAll(pull, `"ref":"main"`, `"ref":"wrong"`)
						}
						if !healthy && req.Method == test.failureMethod && (!test.retrySuccess || mergeCalls == 1) {
							status = test.status
							if test.moved && test.status == http.StatusConflict || test.retryMoved && mergeCalls > 1 {
								runGit(t, fixture.remote, "update-ref", "refs/heads/"+fixture.info.Branch, previous)
							}
							if mergeCalls > 1 && test.retryStatus != 0 {
								status = test.retryStatus
							}
							if test.advanceParallel && mergeCalls == 1 {
								fixture.advanceMain(t, "parallel.txt", "parallel landing\n")
								base = fixture.remoteMain(t)
							}
							if test.advanceOnMerge {
								fixture.advanceMain(t, "feature.txt", "base conflict\n")
								base = fixture.remoteMain(t)
							}
							response = fmt.Sprintf(`{"message":%q}`, test.message)
							if previousRun && test.ownedHeadRetry {
								status, response = http.StatusMethodNotAllowed, `{"message":"Pull Request is not mergeable"}`
							}
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
								published, _, readErr := remoteBranchHead(t.Context(), fixture.info.Path, "origin", fixture.info.Branch)
								if readErr != nil {
									t.Fatal(readErr)
								}
								switch {
								case req.URL.Path == "/repos/example/repo/pulls/7" && !test.external:
									pullHead, pullBase, pullBranch := published, fixture.remoteMain(t), fixture.info.Branch
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
									if test.prepareOnly {
										response = pull
									}
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
									if (test.gitReadFailure != "" || test.baseMovesDuringRead) && !healthy {
										realGit, err := exec.LookPath("git")
										if err != nil {
											t.Fatal(err)
										}
										wrapper := t.TempDir()
										script := "#!/bin/sh\n" +
											"if [ \"$1\" = \"-C\" ] && [ \"$3\" = \"fetch\" ]; then\n" +
											"printf '%s\\n' " + shellQuote("fatal: unable to access github.com: "+test.gitReadFailure) + " >&2\nexit 128\nfi\n" +
											"exec " + shellQuote(realGit) + " \"$@\"\n"
										if test.baseMovesDuringRead {
											script = "#!/bin/sh\n" +
												"if [ \"$1\" = \"-C\" ] && [ \"$3\" = \"ls-remote\" ]; then\n" +
												shellQuote(realGit) + " -C " + shellQuote(fixture.remote) + " update-ref refs/heads/main " + shellQuote(fixture.head) + "\nfi\n" +
												"exec " + shellQuote(realGit) + " \"$@\"\n"
										}
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
								case test.duplicatePull:
									response = "[" + pull + "," + strings.Replace(pull, `"number":7`, `"number":8`, 1) + "]"
								case createdPull:
									response = "[" + pull + "]"
								case test.remoteNewer:
									response = "[" + strings.ReplaceAll(pull, fixture.head, previous) + "]"
								case test.pullState == "open":
									response = "[" + pull + "]"
								case test.prepareOnly && test.pullState == "stale" && published == fixture.head:
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
								createCalls++
								wantBody := "Native Change Request"
								if test.sourceIssues {
									wantBody += "\n\nCloses digitaldrywood/detent#3410"
								}
								if body["body"] != wantBody || body["title"] != "Native Change Request" || body["base"] != target || body["head"] != "example:"+fixture.info.Branch {
									t.Fatalf("PR creation payload = %+v, want body %q", body, wantBody)
								}
								createdPull = true
								response = pull
							case "PATCH":
								wantBody := test.existingBody + "\n\nCloses digitaldrywood/detent#3410"
								if !test.wantPatch || body["body"] != wantBody || len(body) != 1 || req.URL.Path != "/repos/example/repo/pulls/7" {
									t.Fatalf("reused PR payload = %+v, want body %q", body, wantBody)
								}
								response = pull
							case "PUT":
								if test.moved && test.failureMethod == "" {
									runGit(t, fixture.remote, "update-ref", "refs/heads/"+fixture.info.Branch, previous)
								}
								published := strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch))
								if published != body["sha"] {
									status = http.StatusConflict
									response = `{"message":"Head branch was modified. Review and try the merge again."}`
								} else {
									runGit(t, fixture.remote, "update-ref", "refs/heads/main", body["sha"])
									response = fmt.Sprintf(`{"merged":true,"sha":"%s"}`, body["sha"])
								}
							}
						}
						if test.lostCreate && req.Method == http.MethodPost && !createResponseLost {
							createResponseLost = true
							return nil, errors.New("PR create response lost")
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
			opts := LandOptions{HeadSHA: fixture.head, Method: test.method, Repository: repository, Message: "Native Change Request", GitHubClient: client, ValidationCommand: "test -f feature.txt"}
			opts.TargetBranch = test.targetBranch
			if test.rolling {
				opts.LandingMode = gate.LandingRollingBarrier
				opts.ValidationCommand = "exit 19"
			}
			if test.retryHeadMoved {
				opts.ValidationCommand = "test -f feature.txt && if test -f parallel.txt; then git push --force origin " + shellQuote(originalBase+":refs/heads/"+fixture.info.Branch) + "; fi"
			}
			if test.retryGateFailure {
				opts.ValidationCommand = "test -f feature.txt && test ! -f parallel.txt"
			}
			if test.baseMovesDuringGate {
				opts.ValidationCommand = "test -f feature.txt && git push origin " + shellQuote(fixture.head+":refs/heads/main")
			}
			if test.combinedGateFailure {
				opts.ValidationCommand = "test -f feature.txt && if test -f parallel.txt; then printf combined-tree-lint-error; exit 7; fi"
			}
			if test.gateFailure {
				opts.ValidationCommand = "git cat-file -e short-test-failure-sentinel"
			}
			if test.sourceIssues {
				source := tracker.GitHubIssueSourceReference("I_original", "https://github.com/digitaldrywood/detent/issues/3410")
				opts.SourceIssues = []tracker.ExternalReference{source, source}
			}
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
			verifyPreparation := func() {}
			if test.preparation {
				staging := filepath.Join(fixture.backend.root, "landing-"+landingInfo.Key)
				trace := filepath.Join(t.TempDir(), "preparation-trace")
				if err := os.WriteFile(filepath.Join(fixture.source, ".git", "info", "exclude"), []byte(".landing-prerequisite/\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				fixture.backend.hooks.AfterCreate = "test \"$PWD\" = " + shellQuote(staging) + " && mkdir .landing-prerequisite && cp feature.txt .landing-prerequisite/feature && git rev-parse HEAD > .landing-prerequisite/head && git rev-parse HEAD^{tree} > .landing-prerequisite/tree && printf 'after_create\\n' >> " + shellQuote(trace)
				fixture.backend.hooks.BeforeRun = "cmp feature.txt .landing-prerequisite/feature && test \"$(cat .landing-prerequisite/head)\" = \"$(git rev-parse HEAD)\" && printf 'before_run\\n' >> " + shellQuote(trace) + " && touch .landing-prerequisite/ready"
				if test.preparationFailure != "" {
					failure := "; printf preparation-failed; exit 23"
					if test.retryPreparation {
						failure = "; if test -f parallel.txt; then printf preparation-failed; exit 23; fi"
					}
					if test.preparationFailure == "after_create" {
						fixture.backend.hooks.AfterCreate += failure
					} else {
						fixture.backend.hooks.BeforeRun += failure
					}
				}
				opts.ValidationCommand = "test -f .landing-prerequisite/ready && cmp feature.txt .landing-prerequisite/feature && test \"$(cat .landing-prerequisite/head)\" = \"$(git rev-parse HEAD)\" && test \"$(cat .landing-prerequisite/tree)\" = \"$(git rev-parse HEAD^{tree})\" && printf 'gate\\n' >> " + shellQuote(trace) + " && " + opts.ValidationCommand
				verifyPreparation = func() {
					if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("staging preparation left worktree: %v", err)
					}
					if got := runGit(t, fixture.source, "worktree", "list", "--porcelain"); strings.Contains(got, staging) {
						t.Fatalf("staging worktree registration survived: %s", got)
					}
					if _, err := os.Stat(filepath.Join(landingInfo.Path, ".landing-prerequisite")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("preparation touched reviewed source: %v", err)
					}
					if got := strings.TrimSpace(runGit(t, landingInfo.Path, "rev-parse", "HEAD")); got != fixture.head {
						t.Fatalf("preparation moved reviewed head: %s", got)
					}
					want := "after_create\nbefore_run\ngate\n"
					switch test.preparationFailure {
					case "after_create":
						want = "after_create\n"
					case "before_run":
						want = "after_create\nbefore_run\n"
					}
					if test.retryPreparation || test.retrySuccess {
						want = "after_create\nbefore_run\ngate\n" + want
					}
					got, err := os.ReadFile(trace)
					if err != nil || string(got) != want {
						t.Fatalf("preparation/gate order = %q, %v; want %q", got, err, want)
					}
				}
			}
			if test.prepareOnly {
				authorityCalls := 0
				authorityErr := errors.New("publication authority lost; resume under the current source owner")
				if test.ambiguousEffect {
					authorityErr = errors.New("publication has an ambiguous external effect; reconcile its receipt before resuming")
				}
				opts.Authorize = func(context.Context) error {
					authorityCalls++
					if authorityCalls == test.authorityFail {
						return authorityErr
					}
					return nil
				}
				if test.stagedWork {
					if err := os.WriteFile(filepath.Join(landingInfo.Path, "repair.txt"), []byte("existing staged repair\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					runGit(t, landingInfo.Path, "add", "repair.txt")
					if _, err := fixture.backend.FinalizeNativeWork(t.Context(), landingInfo, landingIssue, func(context.Context) error { return nil }); err != nil {
						t.Fatal(err)
					}
					fixture.head = strings.TrimSpace(runGit(t, landingInfo.Path, "rev-parse", "HEAD"))
					opts.HeadSHA = fixture.head
				}
				if test.pushRefused {
					hooks := filepath.Join(fixture.remote, "hooks")
					if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte("#!/bin/sh\nprintf publication-policy-refusal >&2\nexit 1\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if test.wrongRemote != "" {
					runGit(t, fixture.source, "config", "url.file://"+fixture.remote+".insteadOf", "https://github.com/other/repo.git")
				}
				switch test.wrongRemote {
				case "selected":
					opts.Remote = "delivery"
					runGit(t, fixture.source, "remote", "add", opts.Remote, "https://github.com/other/repo.git")
				case "pushurl":
					runGit(t, fixture.source, "config", "remote.origin.pushurl", "https://github.com/other/repo.git")
				}
				publication, err := fixture.backend.PrepareGitHubPublication(t.Context(), landingInfo, landingIssue, opts)
				if test.lostCreate {
					if err == nil || createCalls != 1 {
						t.Fatalf("lost create response did not preserve one publication: %+v, %v, calls=%d", publication, err, createCalls)
					}
					publication, err = fixture.backend.PrepareGitHubPublication(t.Context(), landingInfo, landingIssue, opts)
				}
				if fixture.remoteMain(t) != base || mergeCalls != 0 || strings.TrimSpace(runGit(t, landingInfo.Path, "rev-parse", "HEAD")) != fixture.head || strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+target)) != base {
					t.Fatalf("PR preparation changed source or merged under a human hold: %+v, %v", publication, err)
				}
				if test.authorityFail != 0 {
					if !errors.Is(err, authorityErr) || publication.External.ID != "" {
						t.Fatalf("lost authority yielded a publication receipt: %+v, %v", publication, err)
					}
					if test.authorityFail == 1 && len(methods) != 0 || test.authorityFail == 3 && createCalls != 0 {
						t.Fatalf("publication continued after authority or effect refusal: requests=%v creates=%d", methods, createCalls)
					}
					if test.authorityFail == 4 {
						opts.Authorize = func(context.Context) error { return nil }
						publication, err = fixture.backend.PrepareGitHubPublication(t.Context(), landingInfo, landingIssue, opts)
						if err != nil || publication.External.ID != "7" || createCalls != 1 || mergeCalls != 0 {
							t.Fatalf("receipt authority recovery duplicated the PR: %+v, %v, creates=%d", publication, err, createCalls)
						}
					}
					return
				}
				if test.wantRefusal != "" {
					if test.preservePublication && (preservedBranch != runGit(t, fixture.remote, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"+fixture.info.Branch) || preservedLog != runGit(t, fixture.remote, "reflog", "show", "--format=%H %gs", "refs/heads/"+fixture.info.Branch)) {
						t.Fatalf("refusal overwrote published branch or PR head: old=%s current=%s", previous, runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch))
					}
					if test.wrongRemote != "" && len(methods) != 0 || test.duplicatePull && createCalls != 0 {
						t.Fatalf("unsafe publication issued requests: %v, creates=%d", methods, createCalls)
					}
					var refusal *LandRefusal
					if !errors.As(err, &refusal) || refusal.Kind != test.wantRefusal || publication.External.ID != "" {
						t.Fatalf("unsafe publication was not refused: %+v, %v", publication, err)
					}
					return
				}
				if err != nil || publication.Repository != repository || publication.HeadSHA != fixture.head || publication.BaseRef != target || publication.Branch != landingInfo.Branch || publication.External.ID != "7" || publication.External.URL != repository+"/pull/7" {
					t.Fatalf("publication did not record the exact reviewable PR: %+v, %v", publication, err)
				}
				again, err := fixture.backend.PrepareGitHubPublication(t.Context(), landingInfo, landingIssue, opts)
				wantCreates := 1
				if test.pullState == "stale" || test.pullState == "open" {
					wantCreates = 0
				}
				if err != nil || again != publication || createCalls != wantCreates || mergeCalls != 0 {
					t.Fatalf("publication retry duplicated or merged work: %+v, %v, creates=%d merges=%d", again, err, createCalls, mergeCalls)
				}
				return
			}
			if test.repeatRun {
				result, err := fixture.backend.LandChangeViaGitHub(t.Context(), landingInfo, landingIssue, opts)
				if test.preservePublication && (preservedBranch != runGit(t, fixture.remote, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"+fixture.info.Branch) || preservedLog != runGit(t, fixture.remote, "reflog", "show", "--format=%H %gs", "refs/heads/"+fixture.info.Branch) || mergeCalls != 0) {
					t.Fatal("landing refusal changed published source")
				}

				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalBaseMoved || refusal.BaseSHA != "" || result.Rebased || mergeCalls != 1 {
					t.Fatalf("prior landing did not wait without rewriting: result=%#v err=%v calls=%d", result, err, mergeCalls)
				}
				previousRun, methods, mergeCalls = false, nil, 0
				opts.GitHubClient = newClient(false)
			}
			result, err := fixture.backend.LandChangeViaGitHub(context.Background(), landingInfo, landingIssue, opts)
			verifyPreparation()
			if test.preparationFailure != "" {
				var hook *HookError
				var validation *ValidationError
				var refusal *LandRefusal
				wantMerges := 0
				if test.retryPreparation {
					wantMerges = 1
				}
				if !errors.As(err, &hook) || hook.Hook != test.preparationFailure || hook.ExitCode != 23 || !strings.Contains(hook.Output, "preparation-failed") || errors.As(err, &validation) || errors.As(err, &refusal) || mergeCalls != wantMerges || result.MergeSHA != "" || fixture.remoteMain(t) != base {
					t.Fatalf("preparation failure lost instance ownership or merged: result=%#v err=%v calls=%d", result, err, mergeCalls)
				}
				if test.retryPreparation {
					if got := strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch)); got != fixture.head {
						t.Fatalf("failed refresh published combined head: %s", got)
					}
				} else if result.Gate.Command != "" || len(methods) != 0 {
					t.Fatalf("failed preparation ran gate or published: gate=%#v requests=%v", result.Gate, methods)
				}
				return
			}
			if test.baseMovesDuringGate {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalBaseMoved || refusal.BaseSHA != fixture.head || slices.Contains(methods, http.MethodPut) || result.Gate.ExitCode != 0 || result.Gate.Command != opts.ValidationCommand {
					t.Fatalf("merge accepted a base changed during its gate: result=%#v err=%v methods=%v", result, err, methods)
				}
				return
			}
			if test.sourceConflict && !test.advanceOnMerge {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalConflict || slices.Contains(methods, http.MethodPut) || slices.Contains(methods, http.MethodPost) || fixture.remoteMain(t) != base {
					t.Fatalf("combined source conflict reached the forge: result=%#v err=%v methods=%v", result, err, methods)
				}
				return
			}
			if test.combinedGateFailure {
				var validation *ValidationError
				if !errors.As(err, &validation) || !strings.Contains(validation.Output, "combined-tree-lint-error") || result.Gate.Command != opts.ValidationCommand || result.Gate.ExitCode != 7 || result.Gate.DurationNS <= 0 || result.Gate.HeadSHA == fixture.head || !validLandingHead(result.Gate.TreeSHA) || len(methods) != 0 || fixture.remoteMain(t) != base {
					t.Fatalf("combined gate evidence = %#v, err=%v methods=%v", result, err, methods)
				}
				return
			}
			if test.ownedHeadRetry || test.repeatRun {
				updates := strings.Fields(runGit(t, fixture.remote, "reflog", "show", "--format=%H", "refs/heads/"+fixture.info.Branch))
				wantCalls := 1
				if test.ownedHeadRetry {
					wantCalls = 2
				}
				if len(updates) != 1 || updates[0] != fixture.head || result.Rebased || mergeCalls != wantCalls {
					t.Fatalf("projection refusal rewrote the published head: updates=%v result=%#v calls=%d", updates, result, mergeCalls)
				}
			}
			if test.wantRetry || test.retryGateFailure || test.retryHeadMoved {
				wantCalls := 2
				if test.retryGateFailure || test.retryHeadMoved {
					wantCalls = 1
				}
				if mergeCalls != wantCalls || !result.Rebased {
					t.Fatalf("retry calls=%d result=%#v err=%v", mergeCalls, result, err)
				}
				if got := strings.TrimSpace(runGit(t, landingInfo.Path, "rev-parse", "HEAD")); got != fixture.head {
					t.Fatalf("retry moved reviewed workspace: %s", got)
				}
			}
			if test.retryHeadMoved {
				var refusal *LandRefusal
				published := strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch))
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalBaseMoved || published != originalBase || fixture.remoteMain(t) != base {
					t.Fatalf("retry overwrote a moved published head: result=%#v err=%v published=%s", result, err, published)
				}
				return
			}
			if test.retryGateFailure {
				var validation *ValidationError
				if !errors.As(err, &validation) || fixture.remoteMain(t) != base {
					t.Fatalf("retry gate failure = %#v, %v", result, err)
				}
				return
			}
			if test.gateFailure {
				if result.Gate.Command != opts.ValidationCommand || result.Gate.ExitCode == 0 || result.Gate.DurationNS <= 0 {
					t.Fatalf("failed gate lost receipt: %#v", result)
				}
				var validation *ValidationError
				if !errors.As(err, &validation) || !strings.Contains(validation.Output, "short-test-failure-sentinel") || len(methods) != 0 || fixture.remoteMain(t) != base || result.MergeSHA != "" {
					t.Fatalf("red gate wrote to forge or lost evidence: %v, %v", methods, err)
				}
				if _, exists, lookupErr := remoteBranchHead(t.Context(), fixture.info.Path, "origin", fixture.info.Branch); lookupErr != nil || exists {
					t.Fatalf("red gate published source: %v", lookupErr)
				}
				return
			}
			if test.gitReadFailure != "" {
				availability, ok := forgeavailability.As(err)
				if !ok || availability.Class != test.gitReadClass || availability.Scope.Operation != "git fetch" || !strings.Contains(err.Error(), test.message) || result.MergeSHA != "" || fixture.remoteMain(t) != base {
					t.Fatalf("Git transport failure lost its authentic owner: %#v, %v", result, err)
				}
				return
			}
			if test.baseMovesDuringRead {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalBaseMoved || refusal.BaseSHA != "" || result.Rebased || mergeCalls != 1 || fixture.remoteMain(t) != fixture.head {
					t.Fatalf("base race became a head refusal: result=%#v err=%v calls=%d", result, err, mergeCalls)
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
			if test.refreshStatus == http.StatusNotFound {
				var status *github.StatusError
				var refusal *LandRefusal
				if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound || errors.As(err, &refusal) || !strings.Contains(err.Error(), test.message) || result.MergeSHA != "" || fixture.remoteMain(t) != base {
					t.Fatalf("missing projection changed error ownership: %#v, %v", result, err)
				}
				return
			}
			if test.wantDeferred {
				var status *github.StatusError
				var refusal *LandRefusal
				wantStatus := test.status
				if test.retryStatus != 0 {
					wantStatus = test.retryStatus
				}
				if errors.Is(err, forgeavailability.ErrUnavailable) || !errors.As(err, &status) || status.StatusCode != wantStatus || !strings.Contains(status.Body, test.message) || !errors.As(err, &refusal) || refusal.Kind != LandRefusalBaseMoved || refusal.BaseSHA != "" || result.MergeSHA != "" || fixture.remoteMain(t) != base {
					t.Fatalf("unproven conflict = %#v, %v; base = %s", result, err, fixture.remoteMain(t))
				}
				if !test.wantRetry && !test.ownedHeadRetry && strings.Join(methods, ",") != "GET,PUT,GET" && strings.Join(methods, ",") != "GET,POST,PUT,GET" {
					t.Fatalf("conflict refresh sequence = %v", methods)
				}
				return
			}
			if test.wantRefusal != "" {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != test.wantRefusal || errors.Is(err, forgeavailability.ErrUnavailable) || errors.Is(err, github.ErrRateLimited) || fixture.remoteMain(t) != base || result.MergeSHA != "" {
					t.Fatalf("merge refusal = %#v, %v; base = %s", result, err, fixture.remoteMain(t))
				}
				if test.wantRefusal == LandRefusalConflict && !strings.Contains(refusal.Reason, "feature.txt") {
					t.Fatalf("conflict lost file evidence: %v", refusal)
				}
				if test.wantRefusal == LandRefusalBaseMoved {
					var status *github.StatusError
					if !errors.Is(err, connector.ErrPullRequestBaseOutOfDate) || !errors.As(err, &status) || status.StatusCode != 405 || !strings.Contains(status.Body, test.message) {
						t.Fatalf("base race lost its original atomic refusal: %v", err)
					}
				}
				if test.sourceIssues && test.pullState == "stale" {
					if strings.Join(methods, ",") != "GET" {
						t.Fatalf("stale delivery attribution mutated the PR: %v", methods)
					}
					return
				}
				if test.remoteNewer {
					if strings.Join(methods, ",") != "GET" || !strings.Contains(refusal.Reason, "newer work") || createCalls != 0 || mergeCalls != 0 {
						t.Fatalf("rollback preflight mutated publication: %v, %v", err, methods)
					}
				} else if !test.external && (!strings.Contains(strings.Join(methods, ","), "PUT") || !strings.Contains(refusal.Reason, "GitHub refused")) {
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
			if test.failureMethod != "" && !test.retrySuccess {
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
			if test.rolling {
				if result.Gate.Command != "" {
					t.Fatalf("rolling gate ran: %+v", result.Gate)
				}
			} else if result.Gate.Command != opts.ValidationCommand || result.Gate.ExitCode != 0 || result.Gate.DurationNS <= 0 || !validLandingHead(result.Gate.HeadSHA) || !validLandingHead(result.Gate.TreeSHA) {
				t.Fatalf("successful gate lost receipt: %#v", result)
			}
			wantHead := fixture.head
			if test.retrySuccess && !test.ownedHeadRetry {
				wantHead = strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch))
				if wantHead == fixture.head {
					t.Fatal("retry did not combine reviewed source with the current base")
				}
				wantTree := strings.TrimSpace(runGit(t, fixture.source, "merge-tree", "--write-tree", base, fixture.head))
				if gotTree := strings.TrimSpace(runGit(t, fixture.source, "rev-parse", wantHead+"^{tree}")); gotTree != wantTree || result.Gate.TreeSHA != gotTree {
					t.Fatalf("retry changed reviewed source delta: tree=%s want=%s", gotTree, wantTree)
				}
				if test.method == "rebase" && strings.TrimSpace(runGit(t, fixture.source, "rev-list", "--min-parents=2", base+".."+wantHead)) != "" {
					t.Fatal("rebase delivery acquired a merge commit")
				}
				runGit(t, fixture.source, "merge-base", "--is-ancestor", base, wantHead)
			}
			if result.MergeSHA != wantHead || result.BaseRef != "main" || result.BaseBefore != base || result.Method != test.method || result.AttemptBranchPushed != !test.external || fixture.remoteMain(t) != wantHead {
				t.Fatalf("landing = %#v", result)
			}
			wantCreate := !test.external && test.pullState != "open" && test.pullState != "stale" && test.pullState != "merged" && (!test.rate || test.failureMethod != "PUT")
			wantMerge := test.pullState != "merged"
			calls := strings.Join(methods, ",")
			if strings.Contains(calls, "PATCH") != test.wantPatch {
				t.Fatalf("source attribution operations = %v", methods)
			}
			if strings.Contains(calls, "POST") != wantCreate || strings.Contains(calls, "PUT") != wantMerge {
				t.Fatalf("operations = %v", methods)
			}
			if published := strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch)); published != wantHead {
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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

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
