package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/issuefiling"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeCreateIssueResponse(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"created", http.StatusOK, `{"work_item_id":"wi_created"}`, false},
		{"challenge", http.StatusConflict, `{"code":"possible_duplicates","message":"Possible duplicate issues found","challenge_token":"dup_test","possible_duplicates":[{"number":1}]}`, true},
		{"legacy success challenge", http.StatusOK, `{"challenge_token":"dup_test","possible_duplicates":[{"number":1}]}`, true},
		{"missing issue identity", http.StatusOK, `{}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v2/organizations/org_example/projects/prj_example/work-items" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				if _, err := w.Write([]byte(test.body)); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test-token" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			issue, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create"}, Title: "Example issue", State: "Todo"})
			if (err != nil) != test.wantError {
				t.Fatalf("issue=%#v, error=%v", issue, err)
			}
			if err == nil && issue.WorkItemID != "wi_created" || err != nil && issue.WorkItemID != "" {
				t.Fatalf("unexpected issue identity: %#v", issue)
			}
			if test.status == http.StatusConflict {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Code != "possible_duplicates" {
					t.Fatalf("challenge was not an API error: %v", err)
				}
			}
		})
	}
}

func TestNativeMachineIntakeAcrossBoardRefresh(t *testing.T) {
	const project = "/api/v2/organizations/org_example/projects/prj_example"
	collaborating := []string{runnerauth.Read, runnerauth.Collaborate}
	readOnly := []string{runnerauth.Read}
	for _, test := range []struct {
		name                        string
		cached, current, afterPage  []string
		rotate                      bool
		wantStatus                  int
		wantBoardReads, wantFilings int
	}{
		{"collaboration revoked", collaborating, readOnly, nil, false, http.StatusForbidden, 0, 1},
		{"collaboration granted", readOnly, collaborating, nil, false, http.StatusUnprocessableEntity, 2, 0},
		{"revoked during pagination", readOnly, collaborating, readOnly, false, http.StatusForbidden, 2, 1},
		{"collaboration granted during rotation", readOnly, collaborating, nil, true, http.StatusUnprocessableEntity, 2, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, file := machineIntakeCredential(t, test.cached)
			credential := file.Credential
			operations := test.current
			var boardReads, filings int
			transport := executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer "+credential {
					t.Fatal("request did not use the current credential")
				}
				w := httptest.NewRecorder()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v2/organizations/org_example/runners/"+file.Identity.RunnerID:
					if test.rotate && credential == file.Credential {
						rotated := file
						var err error
						rotated.Credential, err = apikey.GenerateToken()
						if err != nil {
							t.Fatal(err)
						}
						if err := runnerauth.Save(path, rotated); err != nil {
							t.Fatal(err)
						}
						credential = rotated.Credential
						w.WriteHeader(http.StatusUnauthorized)
						break
					}
					identity := file.Identity
					identity.Operations = operations
					if err := json.NewEncoder(w).Encode(identity); err != nil {
						t.Fatal(err)
					}
				case r.Method == http.MethodGet && r.URL.Path == project:
					w.WriteString(`{"states":[{"name":"Backlog"}]}`)
				case r.Method == http.MethodGet && r.URL.Path == project+"/work-items":
					boardReads++
					if r.URL.Query().Get("cursor") == "" {
						w.WriteString(`{"items":[{"work_item_id":"wi_other","body":"Unrelated issue"}],"next_cursor":"previous-sequence"}`)
					} else {
						if test.afterPage != nil {
							operations = test.afterPage
						}
						w.WriteHeader(http.StatusUnprocessableEntity)
						w.WriteString(`{"code":"invalid_request","message":"Work list order changed; restart the query"}`)
					}
				case r.Method == http.MethodPost && r.URL.Path == project+"/work-items":
					filings++
					w.WriteHeader(http.StatusForbidden)
					w.WriteString(`{"code":"forbidden","message":"Collaboration permission required"}`)
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				return w.Result(), nil
			})
			conn := machineIntakeConnector(t, path, file, transport)
			draft := issuefiling.IssueDraft{Title: "Example finding", Body: issueorigin.Stamp("Example observation", issueorigin.Origin{Kind: "worker", Fingerprint: "example-finding"})}
			_, err := conn.CreateIntakeIssue(t.Context(), draft)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != test.wantStatus {
				t.Fatalf("intake error = %v, want HTTP %d", err, test.wantStatus)
			}
			if boardReads != test.wantBoardReads || filings != test.wantFilings {
				t.Fatalf("board reads = %d, filings = %d", boardReads, filings)
			}
			saved, err := runnerauth.Load(path)
			if err != nil || !slices.Equal(saved.Identity.Operations, test.cached) {
				t.Fatalf("saved operations = %v, want %v, error = %v", saved.Identity.Operations, test.cached, err)
			}
		})
	}
}

func TestNativeMachineIntakeGrantCancellation(t *testing.T) {
	path, file := machineIntakeCredential(t, []string{runnerauth.Read})
	lock, err := instancelock.Acquire(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	entered := make(chan struct{})
	conn := machineIntakeConnector(t, path, file, executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/organizations/org_example/runners/"+file.Identity.RunnerID || r.Header.Get("Authorization") != "Bearer "+file.Credential {
			return nil, fmt.Errorf("unexpected grant request %s %s", r.Method, r.URL.Path)
		}
		select {
		case entered <- struct{}{}:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	wait := testenv.WaitContext(t, t.Context())
	var calls sync.WaitGroup
	defer calls.Wait()
	start := func(ctx context.Context) <-chan error {
		done := make(chan error, 1)
		calls.Go(func() {
			draft := issuefiling.IssueDraft{Title: "Example", Body: issueorigin.Stamp("Observation", issueorigin.Origin{Kind: "worker", Fingerprint: "example"})}
			_, err := conn.CreateIntakeIssue(ctx, draft)
			done <- err
		})
		select {
		case err := <-done:
			t.Fatalf("intake returned before the grant read: %v", err)
		case <-entered:
		case <-wait.Done():
			t.Fatalf("grant read did not enter the transport: %v", wait.Err())
		}
		return done
	}
	firstCtx, cancelFirst := context.WithCancel(wait)
	defer cancelFirst()
	first := start(firstCtx)
	secondCtx, cancelSecond := context.WithCancel(wait)
	defer cancelSecond()
	second := start(secondCtx)
	cancelSecond()
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatalf("second grant read = %v", err)
	}
	cancelFirst()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first grant read = %v", err)
	}
}

func machineIntakeCredential(t *testing.T, operations []string) (string, runnerauth.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(path, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	file.Identity.OrganizationID = "org_example"
	file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
	file.Identity.Operations = operations
	if err := runnerauth.Save(path, file); err != nil {
		t.Fatal(err)
	}
	return path, file
}

func machineIntakeConnector(t *testing.T, path string, file runnerauth.File, transport http.RoundTripper) *NativeConnector {
	t.Helper()
	client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_example", "prj_example")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := NewNativeConnector(native)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}
