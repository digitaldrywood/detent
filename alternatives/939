package github

import (
	"context"
	"net/http"
	"testing"
)

func TestConnectorScanIssueExposureListsMatchingIssues(t *testing.T) {
	t.Parallel()

	path := restIssueExposureSearchPath("public/destination", "private/source", 1)
	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		method: http.MethodGet,
		path:   path,
		body:   `{"total_count":1,"items":[{"node_id":"I_42","number":42,"html_url":"https://github.com/public/destination/issues/42"}]}`,
	}})
	connector := newGitHubTestConnector(t, server, Config{})

	findings, err := connector.ScanIssueExposure(context.Background(), "public/destination", []string{"private/source", "private/source"})
	if err != nil {
		t.Fatalf("ScanIssueExposure() error = %v", err)
	}
	if len(findings) != 1 || findings[0].Number != 42 || findings[0].MatchedIdentifier != "private/source" {
		t.Fatalf("findings = %#v", findings)
	}
	if requests := server.requests(); len(requests) != 1 {
		t.Fatalf("request count = %d, want deduplicated search", len(requests))
	}
}
