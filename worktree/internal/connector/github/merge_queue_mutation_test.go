package github

import (
	"strings"
	"testing"
)

func TestMergeQueueMutationsOmitRootRateLimit(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		document string
	}{
		{"enqueue", enqueuePullRequestMutation},
		{"dequeue", dequeuePullRequestMutation},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.HasPrefix(strings.TrimSpace(tt.document), "mutation ") {
				t.Fatalf("document is not a mutation: %q", tt.document)
			}
			if strings.Contains(tt.document, "rateLimit") {
				t.Fatalf("mutation selects rateLimit, which GitHub rejects on the Mutation type:\n%s", tt.document)
			}
		})
	}
}
