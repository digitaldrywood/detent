package web_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web"
)

type changeConnectorProbe struct {
	connectorProbe
	detail tracker.ChangeDetail
}

func (c changeConnectorProbe) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	if len(ids) != 1 || ids[0] != string(c.detail.Change.WorkItemID) {
		return nil, nil
	}
	issue := connector.NewIssue()
	issue.ID, issue.Identifier, issue.Title, issue.State = ids[0], "Native issue #2188", "Native Change Requests", "In Progress"
	return []connector.Issue{issue}, nil
}

func (c changeConnectorProbe) FetchChanges(_ context.Context, _ tracker.NativeWorkItemID) ([]tracker.ChangeRequest, error) {
	return []tracker.ChangeRequest{c.detail.Change}, nil
}

func (c changeConnectorProbe) FetchChange(_ context.Context, _ tracker.NativeWorkItemID, id string) (tracker.ChangeDetail, error) {
	detail := c.detail
	switch id {
	case "change_error":
		return tracker.ChangeDetail{}, errors.New("Hub unavailable")
	case "change_missing":
		return tracker.ChangeDetail{}, &hubclient.APIError{Status: http.StatusNotFound}
	case "change_empty":
		detail.Change.ID, detail.Change.CurrentVersion = id, ""
		detail.Versions, detail.Reviews, detail.Checks, detail.Discussion = nil, nil, nil, nil
		detail.Summary = changerequest.Summarize(detail, "", "", time.Now())
	case "change_no_ci":
		detail.Versions = append([]tracker.ChangeVersion(nil), detail.Versions...)
		version := &detail.Versions[1]
		version.Checks, version.ReviewPolicy.RequiredChecks = nil, nil
		detail.Summary = changerequest.Summarize(detail, version.PolicyID, version.ReviewPolicy.ID, time.Now())
	}
	return detail, nil
}

func (c changeConnectorProbe) FetchNativeAttempts(_ context.Context, _ tracker.NativeWorkItemID) ([]tracker.NativeAttempt, error) {
	return []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{RunID: "run_example", AttemptID: "attempt_example", PolicyID: c.detail.Versions[1].PolicyID, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "model"}}, Status: "succeeded", StartedAt: c.detail.Change.CreatedAt}}, nil
}

type changeSchedulingProbe struct{ source changeConnectorProbe }

func (s changeSchedulingProbe) ConnectorForProject(string) (connector.Connector, bool) {
	return s.source, true
}
func (changeSchedulingProbe) HeartbeatInterval() time.Duration { return time.Minute }
func (changeSchedulingProbe) FetchCandidateIssues(context.Context, orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	return nil, nil
}
func (changeSchedulingProbe) AdoptClaim(context.Context, connector.Issue, time.Time) (orchestrator.Claimed, error) {
	return orchestrator.Claimed{}, connector.ErrNotImplemented
}
func (changeSchedulingProbe) RenewClaim(context.Context, string, time.Time) (orchestrator.Claimed, error) {
	return orchestrator.Claimed{}, connector.ErrNotImplemented
}
func (changeSchedulingProbe) ReleaseClaim(context.Context, string, string) error { return nil }

func changeWebFixture() tracker.ChangeDetail {
	now := time.Now().UTC()
	rules := tracker.ChangeReviewPolicy{ID: "review_" + strings.Repeat("a", 64), PolicyID: "policy_" + strings.Repeat("b", 64), RequireReview: true}
	change := tracker.ChangeRequest{ID: "change_example", WorkItemID: "wi_example", LinkedIssues: []tracker.NativeWorkItemID{"wi_example"}, Title: "Native Change Requests and immutable versions", Body: "Keep review evidence tied to the code that was reviewed.", CurrentVersion: "version_second", CreatedAt: now}
	version := tracker.ChangeVersion{ID: "version_first", ChangeID: change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("a", 40), PolicyID: rules.PolicyID, RunID: "run_example", AttemptID: "attempt_example", Code: tracker.ChangeArtifact{Kind: "code", URI: "s3://customer-code/versions/code.bundle", SHA256: strings.Repeat("b", 64), Availability: "available"}, Artifacts: []tracker.ChangeArtifact{{Kind: "manifest", URI: "s3://customer-code/versions/manifest.json", SHA256: strings.Repeat("c", 64), Availability: "missing"}}}, Policy: policy.Descriptor{ID: rules.PolicyID, ConfigDigest: strings.Repeat("d", 64)}, ReviewPolicy: rules, CreatedAt: now, Checks: []tracker.ChangeCheckExpectation{{ChangeCheckSpec: tracker.ChangeCheckSpec{Name: "Tests", Source: "independent", WorkflowID: "ci.yml", MaxAgeSeconds: 3600}, CheckRunID: "check_first"}}}
	second := version
	second.ID, second.Number, second.HeadSHA = "version_second", 2, strings.Repeat("c", 40)
	second.Checks = []tracker.ChangeCheckExpectation{{ChangeCheckSpec: tracker.ChangeCheckSpec{Name: "Tests", Source: "customer", WorkflowID: "ci.yml", MaxAgeSeconds: 3600}, CheckRunID: "check_second"}}
	detail := tracker.ChangeDetail{Change: change, Versions: []tracker.ChangeVersion{version, second}, Reviews: []tracker.ChangeReview{{ID: "review_first", VersionID: version.ID, Decision: "approved", Actor: tracker.Actor{Kind: "human", PrincipalID: "reviewer"}}}, Discussion: []tracker.ChangeDiscussion{{ID: "native", Body: "Native discussion <script>alert(1)</script>", Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, CreatedAt: now}, {ID: "imported", Body: "Imported review context", VersionID: version.ID, Provenance: &tracker.Provenance{Provider: "github", ExternalID: "123", AuthorID: "contributor", CreatedAt: now}}}}
	detail.Summary = changerequest.Summarize(detail, rules.PolicyID, rules.ID, now)
	return detail
}

func newChangeTestServer(t *testing.T) *web.Server {
	t.Helper()
	source := changeConnectorProbe{connectorProbe: connectorProbe{name: "hub_native"}, detail: changeWebFixture()}
	workflow := workflowconfig.Default()
	workflow.Tracker.Kind = workflowconfig.TrackerHubNative
	tracked, err := project.New(project.Config{Project: globalconfig.Project{ID: "native"}, Workflow: workflowconfig.Workflow{Config: workflow, Prompt: "Work the native issue."}}, project.Dependencies{Scheduling: changeSchedulingProbe{source: source}})
	if err != nil {
		t.Fatal(err)
	}
	registry := project.NewRegistry()
	if err := registry.Set(tracked); err != nil {
		t.Fatal(err)
	}
	snapshots := hub.New[telemetry.Snapshot]()
	if err := snapshots.Publish(telemetry.Snapshot{GeneratedAt: time.Now(), Projects: []telemetry.ProjectSnapshot{{Project: telemetry.Project{ID: "native", DisplayName: "Native work"}}}}); err != nil {
		t.Fatal(err)
	}
	deps := testDeps(t)
	deps.Registry, deps.Hub, deps.Connector, deps.Store = registry, snapshots, source, openWebTestStore(t)
	server, err := web.NewServer(web.Config{StaticDir: "../../static"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestChangeDetailStatesAndNavigation(t *testing.T) {
	t.Parallel()
	server := newChangeTestServer(t)
	for _, test := range []struct {
		name string
		path string
		want []string
	}{
		{"loading", "/changes/change_example", []string{"Loading Change Request", "hx-trigger=\"load\""}},
		{"two versions", "/changes/change_example?content=1", []string{"Version 1", "Version 2", "current", "Approval belongs to an older version", "No GitHub PR linked", "customer validation", "Code and artifacts", "manifest · missing", "imported from github", "native human", "&lt;script&gt;", "/runs/attempt_example", "/projects/native/issues/wi_example"}},
		{"older version", "/changes/change_example?content=1&version=version_first", []string{"You are viewing an older version", "independent validation"}},
		{"invalid version", "/changes/change_example?content=1&version=unknown", []string{"Requested version was not found"}},
		{"empty", "/changes/change_empty?content=1", []string{"No version published", "No discussion yet", "No native decisions"}},
		{"no CI required", "/changes/change_no_ci?content=1", []string{"not required", "The approved policy requires no CI checks for this version", "Approval belongs to an older version"}},
		{"error", "/changes/change_error?content=1", []string{"could not be loaded", "Retry"}},
		{"missing", "/changes/change_missing?content=1", []string{"not found in this project"}},
		{"run", "/runs/attempt_example", []string{"Native run", "succeeded", "run_example", "Change Requests", "/changes/change_example"}},
		{"issue", "", []string{"Native Change Requests", "Change Requests", "/changes/change_example"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := requestHTML(t, server.Handler(), http.MethodGet, "/projects/native/issues/wi_example"+test.path, http.StatusOK)
			for _, want := range test.want {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %q in %s", want, body)
				}
			}
			if strings.Contains(body, "<script>alert(1)</script>") {
				t.Fatal("unescaped discussion")
			}
		})
	}
	requestHTML(t, server.Handler(), http.MethodGet, "/projects/native/issues/wi_example/runs/missing", http.StatusNotFound)
	requestHTML(t, server.Handler(), http.MethodGet, "/projects/missing/issues/wi_example/changes/change_example", http.StatusNotFound)
}
