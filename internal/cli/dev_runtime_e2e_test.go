package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/devruntime"
)

func TestStartIsolatedRuntimeAutoPromotesFixtureAndStopsOnCancel(t *testing.T) {
	runtime, err := devruntime.Build(devruntime.Config{Home: t.TempDir(), Port: 0})
	if err != nil {
		t.Fatalf("devruntime.Build() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- startRunning(ctx, devRuntimeBootConfig(runtime, "127.0.0.1", defaultOptions(), output))
	}()
	t.Cleanup(cancel)

	url := waitForIsolatedRuntimeURL(t, output, done)
	if banner := output.String(); !strings.Contains(banner, "Mode: isolated dev runtime") || !strings.Contains(banner, "DB mode: memory") {
		t.Fatalf("isolated runtime banner missing isolation details:\n%s", banner)
	}
	waitForDashboard(t, url+"/health", done)
	body := waitForDashboardConditionWithRefresh(t, url, url+"/api/v1/state", done, "mock issue promoted to Merging", func(body string) bool {
		return boardStateCountFromBody(t, body, "Merging") == 1
	})
	if !strings.Contains(body, `"status":"running"`) {
		t.Fatalf("state response missing running status:\n%s", body)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startRunning() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for isolated runtime to stop")
	}
}

func TestStartKanbanDemoRendersAndAppliesSafeActions(t *testing.T) {
	const projectID = "demo-project"

	runtime, err := devruntime.Build(devruntime.Config{Home: t.TempDir(), Port: 0, Demo: devruntime.DemoKanban, DemoProjectID: projectID})
	if err != nil {
		t.Fatalf("devruntime.Build() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- startRunning(ctx, devRuntimeBootConfig(runtime, "127.0.0.1", defaultOptions(), output))
	}()
	t.Cleanup(cancel)

	dashboardURL := waitForIsolatedRuntimeURL(t, output, done)
	if banner := output.String(); !strings.Contains(banner, "Demo: kanban") {
		t.Fatalf("isolated runtime banner missing demo name:\n%s", banner)
	}
	waitForDashboard(t, dashboardURL+"/health", done)
	postRuntimeRefresh(t, dashboardURL, done)

	fleetBody := waitForDashboardCondition(t, dashboardURL+"/kanban", done, "fleet kanban demo board", func(body string) bool {
		return strings.Contains(body, `aria-label="Fleet Kanban"`) &&
			strings.Contains(body, `data-project-kanban-visibility-key="fleet"`) &&
			strings.Contains(body, `href="/projects/`+projectID+`/kanban"`) &&
			strings.Contains(body, `href="/projects/docs-site/kanban"`) &&
			strings.Contains(body, "Kanban demo backlog intake") &&
			strings.Contains(body, "Kanban demo todo ready card")
	})
	for _, forbidden := range []string{
		`data-kanban-action="move"`,
		`hx-post="/api/v1/kanban/move"`,
		`id="kanban-feedback"`,
	} {
		if strings.Contains(fleetBody, forbidden) {
			t.Fatalf("fleet kanban demo rendered mutation affordance %q:\n%s", forbidden, fleetBody)
		}
	}

	pageURL := dashboardURL + "/projects/" + projectID + "/kanban"
	body := waitForDashboardCondition(t, pageURL, done, "kanban demo mutation controls", func(body string) bool {
		return strings.Contains(body, "Kanban demo backlog intake") &&
			strings.Contains(body, `data-kanban-action="move"`) &&
			strings.Contains(body, `hx-get="/api/v1/kanban/comment?`) &&
			strings.Contains(body, `data-kanban-drop-state="Todo"`) &&
			strings.Contains(body, `name="project_id" value="`+projectID+`"`)
	})
	for _, want := range []string{"Backlog", "Todo", "In Progress", "Blocked", "Human Review", "Rework", "Merging", "Done", "Cancelled"} {
		if !strings.Contains(body, want) {
			t.Fatalf("kanban page missing lane %q:\n%s", want, body)
		}
	}

	dragDialogValues := url.Values{
		"project_id":    {projectID},
		"issue_id":      {"kanban-demo-backlog"},
		"current_state": {"Backlog"},
		"target_state":  {"Todo"},
		"identifier":    {"digitaldrywood/detent#9510"},
		"title":         {"Kanban demo backlog intake"},
	}
	dragDialogURL := dashboardURL + "/api/v1/kanban/move?" + dragDialogValues.Encode()
	waitForDashboardCondition(t, dragDialogURL, done, "kanban drag move dialog target", func(body string) bool {
		return strings.Contains(body, `hx-post="/api/v1/kanban/move"`) &&
			strings.Contains(body, `name="current_state" value="Backlog"`) &&
			strings.Contains(body, `<option value="Todo" selected>`)
	})

	moveBody := postRuntimeKanbanForm(t, dashboardURL+"/api/v1/kanban/move", done, url.Values{
		"project_id":    {projectID},
		"issue_id":      {"kanban-demo-backlog"},
		"current_state": {"Backlog"},
		"target_state":  {"Todo"},
	})
	for _, want := range []string{
		`id="project-kanban"`,
		"Moved card to Todo.",
		`data-kanban-issue-id="kanban-demo-backlog"`,
		`data-kanban-current-state="Todo"`,
	} {
		if !strings.Contains(moveBody, want) {
			t.Fatalf("immediate Kanban move response missing %q:\n%s", want, moveBody)
		}
	}
	if got := strings.Count(moveBody, `data-kanban-issue-id="kanban-demo-backlog"`); got != 1 {
		t.Fatalf("immediate Kanban move response rendered moved card %d times, want 1:\n%s", got, moveBody)
	}
	postRuntimeRefresh(t, dashboardURL, done)
	waitForDashboardCondition(t, pageURL, done, "backlog card moved to todo", func(body string) bool {
		return strings.Contains(body, `data-kanban-issue-id="kanban-demo-backlog"`) &&
			strings.Contains(body, `data-kanban-current-state="Todo"`)
	})

	postRuntimeKanbanForm(t, dashboardURL+"/api/v1/kanban/comment", done, url.Values{
		"project_id": {projectID},
		"target":     {"issue"},
		"issue_id":   {"kanban-demo-backlog"},
		"body":       {"Safe demo issue comment"},
	})
	postRuntimeKanbanForm(t, dashboardURL+"/api/v1/kanban/comment", done, url.Values{
		"project_id":    {projectID},
		"target":        {"pr"},
		"pr_repository": {"digitaldrywood/detent"},
		"pr_number":     {"9515"},
		"body":          {"Safe demo PR comment"},
	})

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startRunning() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for isolated runtime to stop")
	}
}

func TestStartScreenshotsDemoServesScenarioManifestAndUsage(t *testing.T) {
	runtime, err := devruntime.Build(devruntime.Config{Home: t.TempDir(), Port: 0, Demo: devruntime.DemoScreenshots})
	if err != nil {
		t.Fatalf("devruntime.Build() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- startRunning(ctx, devRuntimeBootConfig(runtime, "127.0.0.1", defaultOptions(), output))
	}()
	t.Cleanup(cancel)

	dashboardURL := waitForIsolatedRuntimeURL(t, output, done)
	if banner := output.String(); !strings.Contains(banner, "Demo: screenshots") || !strings.Contains(banner, "Scenario manifest: /api/v1/demo/scenarios") {
		t.Fatalf("isolated runtime banner missing screenshots metadata:\n%s", banner)
	}
	waitForDashboard(t, dashboardURL+"/health", done)

	manifest := waitForDashboard(t, dashboardURL+"/api/v1/demo/scenarios", done)
	if !strings.Contains(manifest, "fleet-healthy-parallel-work") || !strings.Contains(manifest, "reports-normal-window") {
		t.Fatalf("scenario manifest missing required scenarios:\n%s", manifest)
	}

	state := waitForDashboardHeader(t, dashboardURL+"/api/v1/state", done, "fleet-healthy-parallel-work")
	if !strings.Contains(state, `"status":"running"`) || !strings.Contains(state, `"issue_id":"demo-running-core"`) {
		t.Fatalf("scenario state response missing demo running issue:\n%s", state)
	}

	usage := waitForDashboardHeader(t, dashboardURL+"/api/v1/usage?by=project", done, "api-usage-populated")
	if !strings.Contains(usage, `"events":84`) || !strings.Contains(usage, `"bucket":"billing-api"`) {
		t.Fatalf("scenario usage response missing seeded ledger data:\n%s", usage)
	}

	onboarding := waitForDashboardHeader(t, dashboardURL+"/onboarding", done, "onboarding-write-success")
	if !strings.Contains(onboarding, "Wrote WORKFLOW.md") {
		t.Fatalf("onboarding scenario did not render success state:\n%s", onboarding)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startRunning() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for isolated runtime to stop")
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForIsolatedRuntimeURL(t *testing.T, output *lockedBuffer, done <-chan error) string {
	t.Helper()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before banner: %v", err)
		case <-deadline:
			t.Fatalf("timed out waiting for isolated runtime banner:\n%s", output.String())
		default:
		}

		for line := range strings.SplitSeq(output.String(), "\n") {
			url, ok := strings.CutPrefix(line, "Dashboard: ")
			if ok && strings.TrimSpace(url) != "" {
				return strings.TrimSpace(url)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForDashboardHeader(t *testing.T, rawURL string, done <-chan error, scenario string) string {
	t.Helper()

	client := http.Client{Timeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lastErr error
	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before dashboard response: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		req.Header.Set("X-Detent-Demo-Scenario", scenario)
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatalf("ReadAll() error = %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return string(body)
			}
			lastErr = errors.New(resp.Status)
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out fetching %s with scenario %s: %v", rawURL, scenario, lastErr)
	return ""
}

func postRuntimeRefresh(t *testing.T, url string, done <-chan error) {
	t.Helper()

	client := http.Client{Timeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before refresh: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/v1/refresh", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusAccepted {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out posting runtime refresh to %s", url)
}

func waitForDashboardConditionWithRefresh(
	t *testing.T,
	refreshURL string,
	conditionURL string,
	done <-chan error,
	name string,
	ok func(string) bool,
) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	lastBody := ""
	nextRefresh := time.Time{}
	for ctx.Err() == nil {
		now := time.Now()
		if !now.Before(nextRefresh) {
			postRuntimeRefresh(t, refreshURL, done)
			nextRefresh = now.Add(time.Second)
		}
		body := waitForDashboard(t, conditionURL, done)
		lastBody = body
		if ok(body) {
			return body
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for dashboard condition %q at %s; last body:\n%s", name, conditionURL, lastBody)
	return ""
}

func postRuntimeKanbanForm(t *testing.T, rawURL string, done <-chan error, form url.Values) string {
	t.Helper()

	client := http.Client{Timeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before Kanban action: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatalf("ReadAll() error = %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return string(body)
			}
			t.Fatalf("Kanban action status = %d, want 200; body = %s", resp.StatusCode, string(body))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out posting Kanban action to %s", rawURL)
	return ""
}

func boardStateCountFromBody(t *testing.T, body string, state string) int {
	t.Helper()

	var payload struct {
		Board struct {
			StateDistribution []struct {
				State string `json:"state"`
				Count int    `json:"count"`
			} `json:"state_distribution"`
		} `json:"board"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return 0
	}
	for _, entry := range payload.Board.StateDistribution {
		if entry.State == state {
			return entry.Count
		}
	}
	return 0
}
