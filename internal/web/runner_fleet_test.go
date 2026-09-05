package web_test

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web"
)

type runnerFleetProbe struct {
	mu           sync.Mutex
	fleet        runnerauth.Fleet
	requirements policy.Requirements
	fail         bool
	updates      int
}

func (p *runnerFleetProbe) Fleet(context.Context) (runnerauth.Fleet, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := p.fleet
	result.Runners = slices.Clone(result.Runners)
	return result, nil
}

func (p *runnerFleetProbe) ProjectEligibility(ctx context.Context, project string) (runnerauth.ProjectEligibility, error) {
	fleet, err := p.Fleet(ctx)
	if err != nil {
		return runnerauth.ProjectEligibility{}, err
	}
	result := runnerauth.ProjectEligibility{Project: project, Policy: policy.Descriptor{Requirements: p.requirements, SourceRevision: strings.Repeat("a", 40)}}
	eligible := false
	for _, r := range fleet.Runners {
		exclusions := r.Exclusions("prj_a", p.requirements, false)
		eligible = eligible || len(exclusions) == 0
		result.Runners = append(result.Runners, runnerauth.Eligibility{Runner: r, Exclusions: exclusions})
	}
	if !eligible {
		result.Exclusions = []runnerauth.Exclusion{{Code: "no_eligible_runner", Message: "Work stays queued until an authorized runner matches every selector and has available capacity"}}
	}
	return result, nil
}

func (p *runnerFleetProbe) UpdateRunner(_ context.Context, id string, change runnerauth.RoutingChange) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.fleet.Editable || p.fail {
		return errors.New("routing edit refused")
	}
	for i, r := range p.fleet.Runners {
		if r.RunnerID == id && r.Revision == change.ExpectedRevision {
			p.fleet.Runners[i].Routing = change.Routing
			p.fleet.Runners[i].Revision++
			p.updates++
			return nil
		}
	}
	return errors.New("stale runner")
}

func (p *runnerFleetProbe) UpdateHost(_ context.Context, id tracker.MachineID, change runnerauth.HostChange) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.fleet.Editable || p.fail {
		return errors.New("host edit refused")
	}
	for i, r := range p.fleet.Runners {
		if r.MachineID == id && r.HostRevision == change.ExpectedRevision {
			p.fleet.Runners[i].HostDisplayName = change.DisplayName
			p.fleet.Runners[i].HostCapacity = change.Capacity
			p.fleet.Runners[i].HostRevision++
			p.updates++
			return nil
		}
	}
	return errors.New("stale host")
}

func runnerFleetTestProbe() *runnerFleetProbe {
	return &runnerFleetProbe{fleet: runnerauth.Fleet{Editable: true, Projects: map[string]tracker.ProjectID{"repository-a": "prj_a"}, Runners: []runnerauth.Runner{{
		Binding: runnerauth.Binding{RunnerID: "runner_a", MachineID: "machine_a"}, OrganizationID: "org_a", Revision: 1, HostRevision: 1,
		Routing:         runnerauth.Routing{DisplayName: "Mac builder", Tags: []string{"build", "macos"}, State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{"prj_a"}},
		HostDisplayName: "Studio Mac", Hostname: "studio.local", OS: "darwin", Architecture: "arm64", Health: "online", HostCapacity: 2, ReportedCapacity: 2, Operations: []string{runnerauth.Claim},
	}}}}
}

func newRunnerFleetTestServer(t *testing.T, probe *runnerFleetProbe) *web.Server {
	t.Helper()
	deps := testDeps(t)
	deps.RunnerFleet = probe
	if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	server, err := web.NewServer(web.Config{ServerAddress: "127.0.0.1:7777"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return server
}

func TestRunnerFleetViews(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		setup    func(*runnerFleetProbe)
		query    string
		want     string
		editable bool
	}{
		{"empty", func(p *runnerFleetProbe) { p.fleet.Runners = nil }, "", "No runners enrolled", true},
		{"renamed host", func(p *runnerFleetProbe) { p.fleet.Runners[0].HostDisplayName = "Renamed Studio" }, "?runner=runner_a", "Renamed Studio", true},
		{"offline exact host", func(p *runnerFleetProbe) {
			p.fleet.Runners[0].Health = "offline"
			p.requirements.MachineID = "machine_a"
		}, "?project=repository-a", "runner_offline", true},
		{"no matching tags", func(p *runnerFleetProbe) { p.requirements.RequiredTags = []string{"linux"} }, "?project=repository-a", "selector_no_match", true},
		{"read only", func(p *runnerFleetProbe) { p.fleet.Editable = false }, "", "Mac builder", false},
		{"provider exhaustion", func(p *runnerFleetProbe) {
			p.fleet.Runners[0].ProviderCapacity = []providercapacity.View{{Report: providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "work", Models: []string{"sol"}, MaxConcurrent: 2, ObservedAt: time.Now(), ResetAt: time.Now().Add(time.Hour)}, State: "exhausted", Reason: "Provider account is exhausted"}}
		}, "", "Provider account is exhausted", true},
		{"provider selection", func(p *runnerFleetProbe) {
			p.fleet.Runners[0].Leases = []runnerauth.RunnerLease{{ID: "lease", Title: "Selected work", ProviderReservation: &providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: "code", Backend: "codex", Model: "sol"}, Report: providercapacity.Report{AccountAlias: "work"}, Reason: "Quota is unknown"}}}
		}, "", "Quota is unknown", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			probe := runnerFleetTestProbe()
			test.setup(probe)
			server := newRunnerFleetTestServer(t, probe)
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7777/fleet/runners"+test.query, nil)
			req.RemoteAddr = "127.0.0.1:4444"
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, req)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("status=%d, missing %q", response.Code, test.want)
			}
			if !test.editable && strings.Contains(response.Body.String(), "Save runner") {
				t.Fatal("read-only fleet rendered edit controls")
			}
		})
	}
}

func TestRunnerFleetFormAuthorizationAndConflict(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		authorized bool
		fail       bool
		want       int
		updates    int
	}{
		{"authorized", true, false, http.StatusNoContent, 1},
		{"missing management token", false, false, http.StatusForbidden, 0},
		{"conflicting edit", true, true, http.StatusOK, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			probe := runnerFleetTestProbe()
			probe.fail = test.fail
			server := newRunnerFleetTestServer(t, probe)
			page := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7777/fleet/runners", nil)
			request.RemoteAddr = "127.0.0.1:4444"
			server.Handler().ServeHTTP(page, request)
			match := regexp.MustCompile(`"X-Detent-API-Key-Dashboard":"([^"]+)"`).FindStringSubmatch(html.UnescapeString(page.Body.String()))
			if len(match) != 2 {
				t.Fatal("missing management token")
			}
			form := url.Values{"revision": {"1"}, "display_name": {"Renamed runner"}, "tags": {"macos, build"}, "state": {"draining"}, "capacity_limit": {"1"}, "project_ids": {"prj_a"}}
			request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7777/fleet/runners/runner_a", strings.NewReader(form.Encode()))
			request.RemoteAddr = "127.0.0.1:4444"
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("HX-Request", "true")
			request.Header.Set("HX-Current-URL", "http://127.0.0.1:7777/fleet/runners")
			request.Header.Set("HX-Target", "runner-fleet-feedback")
			if test.authorized {
				request.Header.Set("X-Detent-API-Key-Dashboard", match[1])
				for _, cookie := range page.Result().Cookies() {
					request.AddCookie(cookie)
				}
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != test.want || probe.updates != test.updates {
				t.Fatalf("status=%d updates=%d body=%s", response.Code, probe.updates, response.Body.String())
			}
			if test.fail && !strings.Contains(response.Body.String(), "Runner was not changed") {
				t.Fatal("conflict did not explain how to retry")
			}
		})
	}
}
