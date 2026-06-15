package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadinessProjectItemsReadReportsMissingAccess(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		status: http.StatusForbidden,
		body:   `{"message":"Resource not accessible by integration"}`,
	}})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
	})
	checker := githubReadinessChecker{connector: c}

	got := checker.projectItemsReadCheck(context.Background(), []string{"Todo"})
	if got.Status != ReadinessFail {
		t.Fatalf("Status = %s, want %s: %#v", got.Status, ReadinessFail, got)
	}
	if !strings.Contains(got.Detail, "cannot read ProjectV2 items") {
		t.Fatalf("Detail = %q, want ProjectV2 read failure", got.Detail)
	}
}

func TestReadinessConnectorConfigDefaultsLookupEnvForAppCredentials(t *testing.T) {
	t.Setenv("DETENT_TEST_GITHUB_APP_ID", "123")
	t.Setenv("DETENT_TEST_GITHUB_APP_INSTALLATION_ID", "987")
	t.Setenv("DETENT_TEST_GITHUB_APP_PRIVATE_KEY", "private-key")

	cfg := readinessConnectorConfig(Config{
		GitHubAppID:             "$DETENT_TEST_GITHUB_APP_ID",
		GitHubAppInstallationID: "$DETENT_TEST_GITHUB_APP_INSTALLATION_ID",
		GitHubAppPrivateKey:     "$DETENT_TEST_GITHUB_APP_PRIVATE_KEY",
		TokenSource:             staticTokenSource("token"),
	})

	if !hasGitHubAppCredentials(cfg, cfg.LookupEnv) {
		t.Fatal("hasGitHubAppCredentials() = false, want env-backed credentials detected")
	}
}

func TestReadinessProjectStatusWriteReportsMissingProjectWrite(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			body: `{"data":{"node":{"projectItems":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[{"id":"PVTI_1","project":{"id":"PVT_1"},"statusValue":{"name":"Todo"}}]}}}}`,
		},
		{
			body: `{"data":{"node":{"field":{"id":"PVTSSF_status","options":[{"id":"OPT_todo","name":"Todo"}]}}}}`,
		},
		{
			status: http.StatusForbidden,
			body:   `{"message":"Resource not accessible by integration"}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		ProjectSlug: "PVT_1",
	})
	checker := githubReadinessChecker{connector: c}

	got := checker.projectStatusWriteCheck(context.Background(), readinessProbeIssue{
		ID:  "I_kw1",
		Ref: issueRef{Owner: "digitaldrywood", Name: "detent", Number: 1},
	}, true)
	if got.Status != ReadinessFail {
		t.Fatalf("Status = %s, want %s: %#v", got.Status, ReadinessFail, got)
	}
	if !strings.Contains(got.Detail, "cannot update ProjectV2 item field value") {
		t.Fatalf("Detail = %q, want project write failure", got.Detail)
	}
}

func TestReadinessIssueFieldCheckUsesBoardlessStatusSource(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/orgs/digitaldrywood/issue-fields?per_page=100",
			body:   `[{"id":10,"node_id":"IFSS_status","name":"Status","data_type":"single_select","options":[{"id":1,"name":"Todo","color":"gray"}]}]`,
		},
		{
			method: http.MethodGet,
			body:   `{"total_count":0,"items":[]}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceIssueField,
		Repository:         "digitaldrywood/detent",
		StatusField:        "Status",
	})
	checker := githubReadinessChecker{connector: c}

	got := checker.Check(context.Background(), ReadinessConfig{
		StatusStates: []string{"Todo"},
		ReadStates:   []string{"Todo"},
	})
	if len(got) != 5 {
		t.Fatalf("checks len = %d, want auth, access, mappings, read, repository warning: %#v", len(got), got)
	}
	for _, check := range got {
		if strings.Contains(check.Name, "project") {
			t.Fatalf("check name = %q, want no ProjectV2 checks in issue_field mode", check.Name)
		}
	}
	if got[1].Name != "GitHub issue field access" || got[1].Status != ReadinessOK {
		t.Fatalf("issue field access check = %#v, want OK", got[1])
	}
	if got[2].Name != "GitHub issue field Status mappings" || got[2].Status != ReadinessOK {
		t.Fatalf("issue field mappings check = %#v, want OK", got[2])
	}
	if got[3].Name != "GitHub issue field issue read" || got[3].Status != ReadinessOK {
		t.Fatalf("issue field read check = %#v, want OK", got[3])
	}
}

func TestReadinessIssueFieldStatusWriteReappliesProbeValue(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/issues/1/issue-field-values?per_page=100",
			body:   `[{"issue_field_id":10,"node_id":"IFV_1","data_type":"single_select","single_select_option":{"id":1,"name":"Todo","color":"gray"}}]`,
		},
		{
			method: http.MethodGet,
			path:   "/orgs/digitaldrywood/issue-fields?per_page=100",
			body:   `[{"id":10,"node_id":"IFSS_status","name":"Status","data_type":"single_select","options":[{"id":1,"name":"Todo","color":"gray"}]}]`,
		},
		{
			method: http.MethodPost,
			path:   "/repos/digitaldrywood/detent/issues/1/issue-field-values",
			body:   `[{"issue_field_id":10,"node_id":"IFV_1","data_type":"single_select","single_select_option":{"id":1,"name":"Todo","color":"gray"}}]`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{
		GitHubStatusSource: GitHubStatusSourceIssueField,
		Repository:         "digitaldrywood/detent",
		StatusField:        "Status",
	})
	checker := githubReadinessChecker{connector: c}

	got := checker.issueFieldStatusWriteCheck(context.Background(), readinessProbeIssue{
		ID:  "I_kw1",
		Ref: issueRef{Owner: "digitaldrywood", Name: "detent", Number: 1},
	}, true)
	if got.Status != ReadinessOK {
		t.Fatalf("Status = %s, want %s: %#v", got.Status, ReadinessOK, got)
	}
	if !strings.Contains(got.Detail, "reapplied existing issue field Status value") {
		t.Fatalf("Detail = %q, want issue field write proof", got.Detail)
	}
}

func TestReadinessIssueCommentReportsMissingAccess(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodPost,
		path:   "/repos/digitaldrywood/detent/issues/1/comments",
		status: http.StatusForbidden,
		headers: map[string]string{
			"X-Accepted-GitHub-Permissions": "issues=write",
		},
		body: `{"message":"Resource not accessible by integration"}`,
	}})
	c := newGitHubTestConnector(t, server, Config{})
	checker := githubReadinessChecker{connector: c}

	got := checker.issueCommentWriteCheck(context.Background(), readinessProbeIssue{
		ID:  "I_kw1",
		Ref: issueRef{Owner: "digitaldrywood", Name: "detent", Number: 1},
	}, true)
	if got.Status != ReadinessFail {
		t.Fatalf("Status = %s, want %s: %#v", got.Status, ReadinessFail, got)
	}
	for _, want := range []string{"create issue comments", "issues=write"} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("Detail = %q, want containing %q", got.Detail, want)
		}
	}
}

func TestReadinessAssigneeReportsMissingAccess(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodPost,
		path:   "/repos/digitaldrywood/detent/issues/1/assignees",
		status: http.StatusForbidden,
		headers: map[string]string{
			"X-Accepted-GitHub-Permissions": "issues=write",
		},
		body: `{"message":"Resource not accessible by integration"}`,
	}})
	c := newGitHubTestConnector(t, server, Config{})
	checker := githubReadinessChecker{connector: c}

	got := checker.assigneeWriteCheck(context.Background(), readinessProbeIssue{
		ID:  "I_kw1",
		Ref: issueRef{Owner: "digitaldrywood", Name: "detent", Number: 1},
	}, true)
	if got.Status != ReadinessFail {
		t.Fatalf("Status = %s, want %s: %#v", got.Status, ReadinessFail, got)
	}
	for _, want := range []string{"update issue assignees", "issues=write"} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("Detail = %q, want containing %q", got.Detail, want)
		}
	}
}

func TestReadinessPullRequestChecksReportsMissingAccess(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent",
			body:   `{"default_branch":"main"}`,
		},
		{
			method: http.MethodGet,
			path:   "/repos/digitaldrywood/detent/commits/main/check-runs?per_page=100",
			status: http.StatusForbidden,
			headers: map[string]string{
				"X-Accepted-GitHub-Permissions": "checks=read",
			},
			body: `{"message":"Resource not accessible by integration"}`,
		},
	})
	c := newGitHubTestConnector(t, server, Config{})
	checker := githubReadinessChecker{connector: c}

	got := checker.repositoryPullRequestChecksCheck(context.Background(), "digitaldrywood/detent")
	if got.Status != ReadinessFail {
		t.Fatalf("Status = %s, want %s: %#v", got.Status, ReadinessFail, got)
	}
	for _, want := range []string{"checks=read", "token cannot read endpoint"} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("Detail = %q, want containing %q", got.Detail, want)
		}
	}
}

func TestReadinessGitHubAppSelectedRepositoriesAreCaseInsensitive(t *testing.T) {
	t.Parallel()

	got := missingInstallationRepositories(InstallationTokenDetails{
		RepositorySelection: "selected",
		Repositories: []InstallationRepository{{
			FullName: "DigitalDryWood/Detent",
		}},
	}, []string{"digitaldrywood/detent"})
	if len(got) != 0 {
		t.Fatalf("missingInstallationRepositories() = %#v, want none", got)
	}
}

func TestReadinessGitHubAppInstallationReportsMissingPermissions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	privateKey := testPrivateKeyPEM(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/987/access_tokens" {
			t.Fatalf("path = %s, want installation token path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		response := map[string]any{
			"token":                "installation-token",
			"expires_at":           now.Add(time.Hour).Format(time.RFC3339),
			"repository_selection": "all",
			"permissions": map[string]string{
				"issues":                "read",
				"organization_projects": "read",
				"pull_requests":         "read",
				"checks":                "read",
			},
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
	}))
	t.Cleanup(server.Close)
	checker := githubReadinessChecker{cfg: Config{
		Endpoint:                server.URL + "/graphql",
		HTTPClient:              server.Client(),
		GitHubAppID:             "123",
		GitHubAppInstallationID: "987",
		GitHubAppPrivateKey:     privateKey,
		Now:                     func() time.Time { return now },
	}}

	got := checker.appInstallationCheck(context.Background(), ReadinessConfig{
		RequireProjectStatusWrite: true,
		RequireIssueComments:      true,
		RequirePullRequestRead:    true,
		RequirePullRequestChecks:  true,
		Repositories:              []string{"digitaldrywood/detent"},
	})
	if got.Status != ReadinessFail {
		t.Fatalf("Status = %s, want %s: %#v", got.Status, ReadinessFail, got)
	}
	for _, want := range []string{"Projects: write", "Issues: write"} {
		if !strings.Contains(got.Detail, want) {
			t.Fatalf("Detail = %q, want containing %q", got.Detail, want)
		}
	}
}

func TestReadinessUnconfiguredProbeIssueWarns(t *testing.T) {
	t.Parallel()

	checker := githubReadinessChecker{}
	got := checker.probeReadChecks(context.Background(), ReadinessConfig{
		RequireIssueChildrenRead: true,
		RequireIssueParentsRead:  true,
	}, readinessProbeIssue{}, false)
	if len(got) != 2 {
		t.Fatalf("checks len = %d, want 2: %#v", len(got), got)
	}
	for _, check := range got {
		if check.Status != ReadinessWarn {
			t.Fatalf("%s Status = %s, want %s", check.Name, check.Status, ReadinessWarn)
		}
		if !strings.Contains(check.Detail, "issue-specific read capability not proven") {
			t.Fatalf("%s Detail = %q, want unproven read detail", check.Name, check.Detail)
		}
	}
}

func TestReadinessUnconfiguredWriteProbeWarns(t *testing.T) {
	t.Parallel()

	checker := githubReadinessChecker{}
	got := checker.writeChecks(context.Background(), ReadinessConfig{
		RequireProjectStatusWrite: true,
		RequireIssueComments:      true,
		RequireAssigneeWrite:      true,
		RequireIssueClose:         true,
		ProjectFieldWrites:        []ReadinessProjectFieldWrite{{Name: "Owner"}},
	}, readinessProbeIssue{}, false)
	if len(got) != 5 {
		t.Fatalf("checks len = %d, want 5: %#v", len(got), got)
	}
	for _, check := range got {
		if check.Status != ReadinessWarn {
			t.Fatalf("%s Status = %s, want %s", check.Name, check.Status, ReadinessWarn)
		}
		if !strings.Contains(check.Detail, "write capability not proven") {
			t.Fatalf("%s Detail = %q, want unproven write detail", check.Name, check.Detail)
		}
	}
}
