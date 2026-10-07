package workspace

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalGitLandingCI(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, test := range []struct {
		name         string
		state        string
		changedHead  bool
		label        bool
		prior        bool
		moved        bool
		gateFailed   bool
		baseRefresh  bool
		baseDuringCI bool
		denied       bool
	}{
		{name: "missing status triggers and waits"},
		{name: "present label is reapplied at readiness", label: true},
		{name: "pending status waits after label removal", state: "pending"},
		{name: "durable trigger survives label removal before status propagation", prior: true},
		{name: "failed status refuses landing", state: "failure"},
		{name: "error status refuses landing", state: "error"},
		{name: "exact head green lands after label removal", state: "success"},
		{name: "latest pending supersedes historical success", state: "latest-pending"},
		{name: "changed head retriggers existing label", changedHead: true, label: true},
		{name: "changed head cannot reuse former head success", changedHead: true},
		{name: "moved PR head refuses even with green status", state: "success", moved: true},
		{name: "local gate failure never publishes or triggers", gateFailed: true},
		{name: "base refresh cannot merge a new head with old CI", state: "success", baseRefresh: true},
		{name: "base advance during CI reading refuses unvalidated merge", state: "success", baseDuringCI: true},
		{name: "trigger permission refusal retains CI evidence", denied: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLandingFixture(t)
			repository := "https://github.com/example/repo"
			runGit(t, f.source, "config", "url.file://"+f.remote+".insteadOf", repository+".git")
			runGit(t, f.source, "remote", "set-url", "origin", repository+".git")
			base := f.remoteMain(t)
			if test.changedHead {
				runGit(t, f.info.Path, "push", "origin", base+":refs/heads/"+f.info.Branch)
			}
			label := test.label
			var operations []string
			merges := 0
			client, err := github.NewClient(github.ClientConfig{TokenSource: github.StaticTokenSource(t.Name()), DisableConditionalRequests: true,
				HTTPClient: landingHTTPClient(func(req *http.Request) (*http.Response, error) {
					operations = append(operations, req.Method+" "+req.URL.Path)
					var response any
					status := http.StatusOK
					pull := githubLandingPull{Number: 7, State: "open"}
					pull.Head.SHA, pull.Head.Ref, pull.Head.Repo.FullName = f.head, f.info.Branch, "example/repo"
					pull.Base.SHA, pull.Base.Ref, pull.Base.Repo.FullName = f.remoteMain(t), "main", "example/repo"
					switch {
					case req.URL.Path == "/repos/example/repo/pulls":
						response = []githubLandingPull{pull}
					case req.URL.Path == "/repos/example/repo/pulls/7":
						if test.moved {
							pull.Head.SHA = base
						}
						response = pull
					case strings.HasSuffix(req.URL.Path, "/check-runs"):
						if !strings.Contains(req.URL.Path, "/"+f.head+"/") || req.URL.Query().Get("per_page") != "100" {
							t.Fatalf("checks did not read the exact head: %s", req.URL)
						}
						response = map[string]any{"check_runs": []any{}}
					case strings.HasSuffix(req.URL.Path, "/statuses"):
						if !strings.Contains(req.URL.Path, "/"+f.head+"/") {
							t.Fatalf("status did not read the exact head: %s", req.URL)
						}
						statuses := []map[string]any{}
						if test.state != "" {
							state := test.state
							if state == "latest-pending" {
								state = "pending"
								statuses = append(statuses, map[string]any{"id": 1, "context": "Full CI", "state": "success", "created_at": "2026-10-06T19:00:00Z"})
							}
							statuses = append(statuses, map[string]any{"id": 2, "context": "Full CI", "state": state, "created_at": "2026-10-06T20:00:00Z"})
						}
						if test.baseDuringCI {
							f.advanceMain(t, "parallel.txt", "base changed during CI read\n")
						}
						response = statuses
					case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/labels"):
						response = []map[string]string{}
						if label {
							response = []map[string]string{{"name": "run-full-ci"}}
						}
					case req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/labels/run-full-ci"):
						label = false
						response = []any{}
					case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/labels"):
						var body struct {
							Labels []string `json:"labels"`
						}
						if err := json.NewDecoder(req.Body).Decode(&body); err != nil || !slices.Equal(body.Labels, []string{"run-full-ci"}) {
							t.Fatalf("invalid label payload: %+v, %v", body, err)
						}
						if test.denied {
							status, response = http.StatusForbidden, map[string]string{"message": "Resource not accessible by integration"}
						} else {
							label = true
							response = []map[string]string{{"name": "run-full-ci"}}
						}
					case req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/merge"):
						merges++
						var body map[string]string
						if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body["sha"] != f.head || body["merge_method"] != "squash" {
							t.Fatalf("invalid exact head merge: %+v, %v", body, err)
						}
						if test.baseRefresh {
							f.advanceMain(t, "parallel.txt", "parallel base\n")
							status, response = http.StatusMethodNotAllowed, map[string]string{"message": "Pull Request has merge conflicts"}
						} else {
							runGit(t, f.remote, "update-ref", "refs/heads/main", f.head)
							response = githubLandingMerge{Merged: true, SHA: f.head}
						}
					default:
						t.Fatalf("unexpected request %s %s", req.Method, req.URL)
					}
					encoded, err := json.Marshal(response)
					if err != nil {
						t.Fatal(err)
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
				})})
			if err != nil {
				t.Fatal(err)
			}
			opts := LandOptions{HeadSHA: f.head, Method: "squash", Repository: repository, GitHubClient: client, ValidationCommand: "test -f feature.txt", RequiredStatusChecks: []string{"Full CI"}, CITriggerLabel: "run-full-ci"}
			if test.prior {
				opts.PreviousCI = &tracker.NativeLandingCIReceipt{HeadSHA: f.head, PullRequest: 7, TriggerLabel: "run-full-ci", Triggered: true}
			}
			if test.gateFailed {
				opts.ValidationCommand = "false"
			}
			result, err := f.backend.LandChangeViaGitHub(t.Context(), f.info, f.issue, opts)
			if test.gateFailed {
				var validation *ValidationError
				if !errors.As(err, &validation) || len(operations) != 0 {
					t.Fatalf("local failure published or triggered: %+v, %v, %v", result, err, operations)
				}
				return
			}
			if test.moved {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalHeadMoved || merges != 0 || result.CI != nil {
					t.Fatalf("stale PR used green evidence: %+v, %v", result, err)
				}
				return
			}
			if result.CI == nil || result.CI.HeadSHA != f.head || result.CI.PullRequest != 7 || !slices.Equal(result.CI.RequiredChecks, []string{"Full CI"}) {
				t.Fatalf("CI receipt lost current source: %+v", result)
			}
			if test.state == "failure" || test.state == "error" || test.denied || test.baseRefresh || test.baseDuringCI {
				var refusal *LandRefusal
				kind := LandRefusalProtected
				if test.baseRefresh || test.baseDuringCI {
					kind = LandRefusalBaseMoved
				}
				if !errors.As(err, &refusal) || refusal.Kind != kind || result.MergeSHA != "" || merges != map[bool]int{true: 1, false: 0}[test.baseRefresh] {
					t.Fatalf("CI refusal lost ownership: %+v, %v", result, err)
				}
				if test.baseRefresh && strings.TrimSpace(runGit(t, f.remote, "rev-parse", "refs/heads/"+f.info.Branch)) != f.head {
					t.Fatal("refreshed head was pushed without native review and CI")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.state == "success" {
				if result.CI.State != "success" || result.MergeSHA != f.head || merges != 1 || f.remoteMain(t) != f.head || label {
					t.Fatalf("current green did not genuinely land: %+v", result)
				}
				return
			}
			wantTrigger := test.state == "" && !test.prior
			if result.CI.State != "pending" || result.CI.Triggered != (wantTrigger || test.prior) || result.MergeSHA != "" || merges != 0 || f.remoteMain(t) != base || label != wantTrigger {
				t.Fatalf("pending CI merged or retriggered: %+v, %v", result, operations)
			}
			if wantTrigger {
				ops := strings.Join(operations, "\n")
				if !strings.Contains(ops, "POST /repos/example/repo/issues/7/labels") || test.label && !strings.Contains(ops, "DELETE /repos/example/repo/issues/7/labels/run-full-ci") {
					t.Fatalf("missing readiness trigger: %s", ops)
				}
			}
		})
	}
}
