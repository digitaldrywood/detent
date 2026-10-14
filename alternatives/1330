package hubclient

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// These are application effects persisted in native_commands, not a transport
// dedup map. The lost response occurs after the real hub commits the resource.
func TestNativeMutationRetryAfterResponseLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	h := newNativeChangeHub(t)
	var drop atomic.Bool
	original := h.admin.client.httpClient.Transport
	if original == nil {
		original = http.DefaultTransport
	}
	h.admin.client.httpClient.Transport = executionRoundTrip(func(req *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(req)
		if err == nil && req.Method == http.MethodPost && drop.Swap(false) {
			if err := response.Body.Close(); err != nil {
				return nil, err
			}
			return nil, errors.New("lost response credential-sensitive-sentinel")
		}
		return response, err
	})
	source, err := NewNativeConnector(h.admin)
	if err != nil {
		t.Fatal(err)
	}
	metadata := mutation.Metadata{PrincipalID: "operator", OrganizationID: string(h.organization), ProjectID: string(h.project), Action: "file_issue", Source: "mcp", Confirmation: "none", CorrelationID: "first-call"}
	metadata, err = metadata.Bind("reconnect-key", map[string]string{"title": "issue", "body": "sensitive-body-sentinel"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := mutation.WithContext(t.Context(), metadata)
	drop.Store(true)
	draft := connector.IssueDraft{Title: "issue", Body: "sensitive-body-sentinel" + issueContractTestSections}
	if _, err := source.CreateIssue(ctx, draft); err == nil {
		t.Fatal("response loss not injected")
	}
	// Reconstruct the adapter, retaining business identity while correlation changes.
	source, err = NewNativeConnector(h.admin)
	if err != nil {
		t.Fatal(err)
	}
	metadata.CorrelationID = "reconnected-call"
	ctx = mutation.WithContext(t.Context(), metadata)
	issue, err := source.CreateIssue(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			got, err := source.CreateIssue(ctx, draft)
			if err != nil || got.ID != issue.ID {
				t.Errorf("concurrent issue=%+v %v", got, err)
			}
		})
	}
	wg.Wait()
	issues, err := h.admin.Issues(t.Context(), nil)
	if err != nil || len(issues.Items) != 1 {
		t.Fatalf("duplicate issues=%+v %v", issues, err)
	}
	changed := draft
	changed.Body = "changed"
	if _, err := source.CreateIssue(ctx, changed); err == nil {
		t.Fatal("changed payload replayed")
	}
	issueMetadata := metadata
	metadata.Action = "comment"
	metadata, err = metadata.Bind("comment-key", map[string]string{"body": "sensitive-comment-sentinel"})
	if err != nil {
		t.Fatal(err)
	}
	ctx = mutation.WithContext(t.Context(), metadata)
	drop.Store(true)
	if err := source.CreateComment(ctx, issue.ID, "sensitive-comment-sentinel"); err == nil {
		t.Fatal("comment response loss not injected")
	}
	for range 8 {
		wg.Go(func() {
			if err := source.CreateComment(ctx, issue.ID, "sensitive-comment-sentinel"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	comments, err := h.admin.Comments(t.Context(), tracker.NativeWorkItemID(issue.ID), "")
	if err != nil || len(comments.Items) != 1 {
		t.Fatalf("duplicate comments=%+v %v", comments, err)
	}
	if err := source.CreateComment(ctx, issue.ID, "changed"); err == nil {
		t.Fatal("changed comment replayed")
	}
	// Revocation/grant checks still precede a durable replay.
	ctx = mutation.WithContext(t.Context(), issueMetadata)
	h.admin.client.tokenSource = func() string { return "revoked-credential-sentinel" }
	if _, err := source.CreateIssue(ctx, draft); err == nil {
		t.Fatal("replay ignored current authority")
	} else if strings.Contains(err.Error(), "revoked-credential-sentinel") {
		t.Fatal("credential in native error")
	}
}
