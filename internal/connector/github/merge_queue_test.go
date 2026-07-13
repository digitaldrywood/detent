package github

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestConnectorInspectPullRequestMergeQueue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		response  string
		available bool
		wantEntry *connector.PullRequestMergeQueueEntry
	}{
		{
			name:      "detects merge queue policy",
			response:  `{"data":{"repository":{"pullRequest":{"id":"PR_42","mergeStateStatus":"CLEAN","mergeQueue":{"url":"https://github.test/example/repo/queue/main"},"mergeQueueEntry":null}}}}`,
			available: true,
		},
		{
			name:      "falls back without merge queue policy",
			response:  `{"data":{"repository":{"pullRequest":{"id":"PR_42","mergeStateStatus":"CLEAN","mergeQueue":null,"mergeQueueEntry":null}}}}`,
			available: false,
		},
		{
			name:      "recognizes existing queue entry",
			response:  `{"data":{"repository":{"pullRequest":{"id":"PR_42","mergeStateStatus":"BLOCKED","mergeQueueEntry":{"id":"MQE_42","state":"AWAITING_CHECKS","position":2,"estimatedTimeToMerge":420,"enqueuedAt":"2026-07-13T18:00:00Z","mergeQueue":{"url":"https://github.test/example/repo/queue/main","entries":{"totalCount":5}}}}}}}`,
			available: true,
			wantEntry: &connector.PullRequestMergeQueueEntry{
				ID:                          "MQE_42",
				State:                       "AWAITING_CHECKS",
				Position:                    2,
				Depth:                       5,
				EstimatedTimeToMergeSeconds: 420,
				EnqueuedAt:                  mergeQueueTestTime("2026-07-13T18:00:00Z"),
				URL:                         "https://github.test/example/repo/queue/main",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := newGraphQLTestServer(t, []graphqlTestResponse{{body: tt.response}})
			client := newGitHubTestConnector(t, server, Config{})
			prNumber := 42
			status, err := client.InspectPullRequestMergeQueue(context.Background(), connector.Issue{
				Identifier:   "example/repo#1",
				PRNumber:     &prNumber,
				PRRepository: "example/repo",
			})
			if err != nil {
				t.Fatalf("InspectPullRequestMergeQueue() error = %v", err)
			}
			if status.Available != tt.available || status.PullRequestNodeID != "PR_42" {
				t.Fatalf("status = %#v, want available %t and node PR_42", status, tt.available)
			}
			if !reflect.DeepEqual(status.Entry, tt.wantEntry) {
				t.Fatalf("entry = %#v, want %#v", status.Entry, tt.wantEntry)
			}
			request := server.requests()[0]
			query := request["query"].(string)
			if !strings.Contains(query, "mergeQueue { url }") || strings.Contains(query, "mergeStateStatus") {
				t.Fatalf("query = %q, want mergeQueue capability field without mergeStateStatus", query)
			}
			variables := request["variables"].(map[string]any)
			if variables["owner"] != "example" || variables["name"] != "repo" || variables["number"] != float64(42) {
				t.Fatalf("variables = %#v, want example/repo#42", variables)
			}
		})
	}
}

func TestConnectorEnqueuePullRequest(t *testing.T) {
	t.Parallel()

	server := newGraphQLTestServer(t, []graphqlTestResponse{{
		body: `{"data":{"enqueuePullRequest":{"mergeQueueEntry":{"id":"MQE_42","state":"QUEUED","position":3,"estimatedTimeToMerge":300,"enqueuedAt":"2026-07-13T18:00:00Z","mergeQueue":{"url":"https://github.test/example/repo/queue/main","entries":{"totalCount":6}}}}}}`,
	}})
	client := newGitHubTestConnector(t, server, Config{})
	entry, err := client.EnqueuePullRequest(context.Background(), connector.Issue{
		Identifier:   "example/repo#1",
		PRRepository: "example/repo",
		PullRequest: &connector.PullRequest{
			NodeID: "PR_42",
			Number: 42,
		},
	})
	if err != nil {
		t.Fatalf("EnqueuePullRequest() error = %v", err)
	}
	if entry.ID != "MQE_42" || entry.State != "QUEUED" || entry.Position != 3 || entry.Depth != 6 || entry.EstimatedTimeToMergeSeconds != 300 {
		t.Fatalf("entry = %#v, want queued position 3 of 6 with 300s ETA", entry)
	}
	request := server.requests()[0]
	if !strings.Contains(request["query"].(string), "enqueuePullRequest") {
		t.Fatalf("query = %q, want enqueuePullRequest mutation", request["query"])
	}
	variables := request["variables"].(map[string]any)
	if variables["pullRequestId"] != "PR_42" {
		t.Fatalf("pullRequestId = %v, want PR_42", variables["pullRequestId"])
	}
}

func mergeQueueTestTime(value string) *time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return &parsed
}
