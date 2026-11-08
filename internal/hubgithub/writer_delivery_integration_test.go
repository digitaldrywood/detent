package hubgithub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

var errOutboundLostResponse = errors.New("example response lost after commit")

type lostOutboundResponse struct {
	mu       sync.Mutex
	comments []restComment
	writes   int
	started  chan bool
	release  chan struct{}
}

func (c *lostOutboundResponse) RESTPage(_ context.Context, _ string, output any) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := json.Marshal(c.comments)
	if err != nil {
		return "", err
	}
	return "", json.Unmarshal(raw, output)
}

func (c *lostOutboundResponse) REST(ctx context.Context, method, _ string, input, output any) error {
	if method != http.MethodPost {
		return fmt.Errorf("unexpected method %s", method)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	c.mu.Lock()
	c.writes++
	first := c.writes == 1
	c.mu.Unlock()
	if first {
		_, bounded := ctx.Deadline()
		c.started <- bounded
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	comment := restComment{ID: int64(len(c.comments) + 1), Body: body.Body}
	c.comments = append(c.comments, comment)
	c.mu.Unlock()
	if first {
		return errOutboundLostResponse
	}
	raw, err = json.Marshal(comment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, output)
}

func postgresOutboundFixture(t *testing.T, client restClient, now func() time.Time) (*hubserver.Service, tenantstore.Repository, hubserver.Config) {
	t.Helper()
	cfg, err := gitHubServiceConfig(t, hubserver.Config{InitialAdminToken: []byte("example-outbound-admin"), OutboxBackend: NewWriter(client), Clock: now, OutboxPollInterval: time.Hour, OutboxProcessingTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, os.DirFS("../hubserver/postgres/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	service, err := hubserver.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return service, repository, cfg
}

func TestPostgresWriterReconcilesExpiredLostResponse(t *testing.T) {
	client := &lostOutboundResponse{started: make(chan bool, 1), release: make(chan struct{})}
	var clock atomic.Int64
	clock.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	service, repository, cfg := postgresOutboundFixture(t, client, now)
	raw, err := json.Marshal(hubserver.WorkpadDesired{Body: "Example summary", Marker: "<!-- example-summary -->", Summary: true})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := eventstore.NewContent(cfg.RunDetails).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org_example"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), "org_example", corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: "org_example", ID: "project", Name: "Example", Profile: "native"}); err != nil {
			return err
		}
		return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "project", WorkItemID: "wi_example", Key: "lost-response", Repository: "example-org/example-project", IssueNumber: 1, Kind: string(hubserver.MutationWorkpad), CreatedAt: now(), Payload: payload})
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	joined := false
	var release sync.Once
	done := make(chan error, 1)
	t.Cleanup(func() {
		release.Do(func() { close(client.release) })
		cancel()
		if !joined {
			<-done
		}
	})
	go func() { _, err := service.ProcessOutbox(ctx); done <- err }()
	select {
	case bounded := <-client.started:
		if !bounded {
			t.Error("external write has no lease deadline")
		}
	case err := <-done:
		joined = true
		t.Fatalf("writer stopped before publication: %v", err)
	}
	clock.Add(int64(2 * time.Hour))
	if processed, err := service.ProcessOutbox(t.Context()); processed || !errors.Is(err, hubserver.ErrOutboxUncertain) {
		t.Errorf("expired replacement wrote before reconciliation: processed=%t err=%v", processed, err)
	}
	release.Do(func() { close(client.release) })
	if err := <-done; !errors.Is(err, errOutboundLostResponse) {
		t.Errorf("lost-response result=%v", err)
	}
	joined = true
	clock.Add(int64(time.Hour))
	if processed, err := service.ProcessOutbox(t.Context()); !processed || err != nil {
		t.Errorf("stable identity reconciliation processed=%t err=%v", processed, err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.writes != 1 || len(client.comments) != 1 {
		t.Fatalf("expired execution duplicated external effect: writes=%d comments=%d", client.writes, len(client.comments))
	}
}

type boundedOutboundREST struct {
	*scriptedRESTClient
	pages int
}

func (c *boundedOutboundREST) RESTPage(ctx context.Context, path string, output any) (string, error) {
	c.pages++
	return c.scriptedRESTClient.RESTPage(ctx, path, output)
}

func TestPostgresWriterBoundsMergeHistory(t *testing.T) {
	for _, test := range []struct {
		name             string
		blocked, changed bool
	}{{"latest facts permit merge", false, false}, {"later review refuses merge", true, false}, {"changed source revalidates facts", true, true}} {
		t.Run(test.name, func(t *testing.T) {
			ready := true
			pull := testPullRequest("open", "abc123", false, &ready)
			pull.UpdatedAt = time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
			finalPull := pull
			if test.changed {
				finalPull.UpdatedAt = finalPull.UpdatedAt.Add(time.Minute)
			}
			review := testReview("APPROVED", "reviewer")
			if test.blocked {
				review.State = "CHANGES_REQUESTED"
			}
			base := "/repos/example-org/example-project"
			pullPath, reviews, checks, statuses := base+"/pulls/29", base+"/pulls/29/reviews?per_page=100", base+"/commits/abc123/check-runs?per_page=100", base+"/commits/abc123/statuses?per_page=100"
			steps := []restStep{
				{method: http.MethodGet, path: pullPath, response: pull},
				{method: http.MethodGet, path: reviews, response: []restReview{testReview("CHANGES_REQUESTED", "reviewer")}, next: reviews + "&page=2"},
				{method: http.MethodGet, path: reviews + "&page=2", response: []restReview{review}},
				{method: http.MethodGet, path: checks, response: restCheckRuns{CheckRuns: []restCheckRun{{ID: 10, Name: "verify", Status: "completed", Conclusion: "failure"}}}, next: checks + "&page=2"},
				{method: http.MethodGet, path: checks + "&page=2", response: restCheckRuns{CheckRuns: []restCheckRun{{ID: 11, Name: "verify", Status: "completed", Conclusion: "success"}}}},
				{method: http.MethodGet, path: statuses, response: []restStatus{{Context: "build", State: "success"}}, next: statuses + "&page=2"},
				{method: http.MethodGet, path: statuses + "&page=2", response: []restStatus{{Context: "build", State: "failure"}}},
				{method: http.MethodGet, path: pullPath, response: finalPull},
			}
			count := 8
			if test.changed {
				steps = append(steps, restStep{method: http.MethodGet, path: reviews, response: []restReview{}}, restStep{method: http.MethodGet, path: checks, response: restCheckRuns{}}, restStep{method: http.MethodGet, path: statuses, response: []restStatus{}}, restStep{method: http.MethodGet, path: pullPath, response: finalPull})
				count = 12
			}
			if !test.blocked || test.changed {
				steps = append(steps, restStep{method: http.MethodPut, path: pullPath + "/merge", body: map[string]string{"merge_method": "squash", "sha": "abc123"}, response: restMergeResponse{Merged: true}})
			}
			client := &boundedOutboundREST{scriptedRESTClient: &scriptedRESTClient{t: t, steps: steps}}
			var clock atomic.Int64
			clock.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			service, repository, cfg := postgresOutboundFixture(t, client, now)
			raw, err := json.Marshal(hubserver.MergePullRequestDesired{PullRequestNumber: 29, HeadSHA: "abc123", MergeMethod: "squash"})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := eventstore.NewContent(cfg.RunDetails).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org_example"}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), "org_example", corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: "org_example", ID: "project", Name: "Example", Profile: "native"}); err != nil {
					return err
				}
				return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "project", WorkItemID: "wi_example", Key: "merge", Repository: "example-org/example-project", IssueNumber: 1, Kind: string(hubserver.MutationMergePullRequest), CreatedAt: now(), Payload: payload})
			}); err != nil {
				t.Fatal(err)
			}
			for step := 0; step < count; step++ {
				before := client.pages
				processed, err := service.ProcessOutbox(t.Context())
				wantError := test.blocked && !test.changed && step == count-1
				if (err != nil) != wantError || processed == wantError {
					t.Fatalf("step=%d processed=%t err=%v", step, processed, err)
				}
				if client.pages-before > 1 {
					t.Fatalf("one tick read %d history pages", client.pages-before)
				}
				clock.Add(int64(2 * time.Hour))
			}
			client.assertDone()
			if err := repository.ReadTenant(t.Context(), "org_example", corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.(projectstore.GitHubOutboxWriter).GitHubOutbound(t.Context(), "merge")
				if err == nil && (row.Status == "completed") != (!test.blocked || test.changed) {
					t.Fatalf("outbound completion=%+v", row)
				}
				if err == nil && test.changed && (row.Progress.Verification != 2 || row.Blocked) {
					t.Fatalf("new source retained stale verification: %+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
