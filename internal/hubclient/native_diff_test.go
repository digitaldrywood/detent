package hubclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// The runner posts the stored attempt diff before the run event that
// references it (decisions section 18.5).

const testAttemptID = "attempt_00000000000000000000000000000001"

// diffHub records every diff post and answers with the status a test scripts.
type diffHub struct {
	posts    []tracker.AttemptDiffRequest
	statuses []int
}

func newDiffClient(t *testing.T, hub *diffHub) *NativeClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/organizations/org_test/projects/prj_test/attempts/"+testAttemptID+"/diff" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request tracker.AttemptDiffRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		hub.posts = append(hub.posts, request)
		status := http.StatusAccepted
		if len(hub.statuses) > 0 {
			status, hub.statuses = hub.statuses[0], hub.statuses[1:]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusAccepted {
			_ = json.NewEncoder(w).Encode(tracker.AttemptDiffReceipt{Accepted: true, DiffID: "diff_1", Generation: request.Generation, FileCount: len(request.Files)})
			return
		}
		code := "stale_generation"
		if status == http.StatusRequestEntityTooLarge {
			code = "diff_too_large"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": "refused"})
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	return native
}

func diffRequest(files ...tracker.AttemptDiffFile) tracker.AttemptDiffRequest {
	return tracker.AttemptDiffRequest{
		Producer:   tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: testAttemptID, LeaseID: "lease", FencingToken: 3},
		Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 2},
		BaseSHA:    strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Files: files,
	}
}

// The client normalizes before it posts, so a denied patch never leaves the
// runner and a patch past the per-file cap is cut before the wire.
func TestPostAttemptDiffNormalizesBeforePosting(t *testing.T) {
	t.Parallel()
	hub := &diffHub{}
	client := newDiffClient(t, hub)
	receipt, err := client.PostAttemptDiff(t.Context(), testAttemptID, diffRequest(
		tracker.AttemptDiffFile{Path: ".env", Status: tracker.DiffStatusAdded, Additions: 1, Patch: "+TOKEN=secret"},
		tracker.AttemptDiffFile{Path: "big.go", Status: tracker.DiffStatusModified, Patch: strings.Repeat("x", tracker.MaxDiffPatchBytes+8)},
	))
	if err != nil {
		t.Fatalf("PostAttemptDiff: %v", err)
	}
	if !receipt.Accepted || receipt.DiffID != "diff_1" {
		t.Fatalf("receipt = %#v", receipt)
	}
	if len(hub.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(hub.posts))
	}
	posted := hub.posts[0].Files
	if !posted[0].Denied || posted[0].Patch != "" || posted[0].Additions != 1 {
		t.Fatalf("denied file reached the wire as %#v", posted[0])
	}
	if !posted[1].Truncated || len(posted[1].Patch) != tracker.MaxDiffPatchBytes {
		t.Fatalf("oversized patch reached the wire as %d bytes", len(posted[1].Patch))
	}
}

// "A diff over 20 MB is rejected with diff_too_large and the runner posts the
// file list without patches."
func TestPostAttemptDiffRetriesWithoutPatchesWhenTooLarge(t *testing.T) {
	t.Parallel()
	hub := &diffHub{statuses: []int{http.StatusRequestEntityTooLarge, http.StatusAccepted}}
	client := newDiffClient(t, hub)
	if _, err := client.PostAttemptDiff(t.Context(), testAttemptID, diffRequest(
		tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified, Additions: 7, Patch: "@@ big"},
	)); err != nil {
		t.Fatalf("PostAttemptDiff: %v", err)
	}
	if len(hub.posts) != 2 {
		t.Fatalf("posts = %d, want 2", len(hub.posts))
	}
	if hub.posts[0].Files[0].Patch == "" {
		t.Fatal("the first post carries the patches")
	}
	retried := hub.posts[1].Files[0]
	if retried.Patch != "" || !retried.Truncated || retried.Additions != 7 {
		t.Fatalf("retry = %#v", retried)
	}
}

// A refusal that is not diff_too_large is reported, not retried.
func TestPostAttemptDiffReportsOtherFailures(t *testing.T) {
	t.Parallel()
	hub := &diffHub{statuses: []int{http.StatusConflict}}
	client := newDiffClient(t, hub)
	if _, err := client.PostAttemptDiff(t.Context(), testAttemptID, diffRequest(
		tracker.AttemptDiffFile{Path: "a.go", Status: tracker.DiffStatusModified},
	)); err == nil {
		t.Fatal("a refused diff must be reported")
	}
	if len(hub.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(hub.posts))
	}
}

func TestNativeAttemptDiffPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		attempt string
		want    string
		wantErr bool
	}{
		{name: "typed id", attempt: testAttemptID, want: "/attempts/" + testAttemptID + "/diff"},
		{name: "wrong prefix", attempt: "run_1", wantErr: true},
		{name: "path traversal", attempt: "attempt_../../x", wantErr: true},
		{name: "empty", attempt: "", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := nativeAttemptDiffPath(test.attempt)
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Fatalf("path = %q, want %q", got, test.want)
			}
		})
	}
}
