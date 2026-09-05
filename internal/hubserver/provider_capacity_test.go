package hubserver

import (
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func capacityReport(now time.Time) providercapacity.Report {
	return providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "local", SharedAccountAlias: "team", Models: []string{"test-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now}
}

func publishCapacity(t *testing.T, f nativeFixture, r runnerFixture, reports ...providercapacity.Report) {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
		map[string]any{"display_name": "runner", "capacity": 8, "version": "test", "provider_reports": reports})
	requireNativeStatus(t, response, http.StatusNoContent)
}

func providerClaim(r runnerFixture, issue tracker.NativeIssue, session string) tracker.NativeClaim {
	return tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: session, TTLSeconds: 90,
		ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
		ProviderCandidates: []tracker.NativeCapacityCandidate{{WorkItemID: issue.WorkItemID, Revision: issue.Revision, Requirement: providercapacity.Requirement{Role: "implement", Backend: "codex", Model: "test-model"}}}}
}

func TestProviderCapacityClaimObservations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*providercapacity.Report, *tracker.NativeClaim)
		want   int
	}{
		{"available", func(*providercapacity.Report, *tracker.NativeClaim) {}, http.StatusOK},
		{"unknown", func(r *providercapacity.Report, _ *tracker.NativeClaim) { r.Availability = "unknown" }, http.StatusOK},
		{"stale exhaustion", func(r *providercapacity.Report, _ *tracker.NativeClaim) {
			r.Availability = "exhausted"
			r.ObservedAt = r.ObservedAt.Add(-providercapacity.MaxAge)
		}, http.StatusOK},
		{"future observation", func(r *providercapacity.Report, _ *tracker.NativeClaim) {
			r.Availability = "exhausted"
			r.ObservedAt = r.ObservedAt.Add(time.Second)
		}, http.StatusOK},
		{"exhausted", func(r *providercapacity.Report, _ *tracker.NativeClaim) { r.Availability = "exhausted" }, http.StatusConflict},
		{"reset", func(r *providercapacity.Report, _ *tracker.NativeClaim) {
			r.Availability = "exhausted"
			r.ResetAt = r.ObservedAt
			r.ObservedAt = r.ObservedAt.Add(-time.Minute)
		}, http.StatusOK},
		{"explicit model unavailable", func(_ *providercapacity.Report, c *tracker.NativeClaim) {
			c.ProviderCandidates[0].Requirement.Model = "expensive"
		}, http.StatusConflict},
		{"wrong backend", func(_ *providercapacity.Report, c *tracker.NativeClaim) {
			c.ProviderCandidates[0].Requirement.Backend = "other"
		}, http.StatusConflict},
		{"changed issue", func(_ *providercapacity.Report, c *tracker.NativeClaim) { c.ProviderCandidates[0].Revision++ }, http.StatusConflict},
		{"missing requirements", func(_ *providercapacity.Report, c *tracker.NativeClaim) { c.ProviderCandidates = nil }, http.StatusConflict},
		{"invalid requirements", func(_ *providercapacity.Report, c *tracker.NativeClaim) {
			c.ProviderCandidates[0].Requirement.Model = ""
		}, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			f := newNativeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}), "", "provider")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			report := capacityReport(now)
			claim := providerClaim(r, issue, "work")
			test.change(&report, &claim)
			publishCapacity(t, f, r, report)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, test.want)
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM provider_reservations").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if test.want != http.StatusOK {
				if count != 0 {
					t.Fatal("failed claim retained reservation")
				}
				return
			}
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			if count != 1 || lease.ProviderReservation == nil || lease.ProviderReservation.Model != "test-model" {
				t.Fatalf("lease = %+v", lease)
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var repeated tracker.NativeLease
			decodeHubResponse(t, response, &repeated)
			if repeated.ID != lease.ID || repeated.ProviderReservation.Model != lease.ProviderReservation.Model {
				t.Fatal("idempotent claim changed reservation")
			}
		})
	}
}

func TestProviderSharedCapacityConcurrentRecovery(t *testing.T) {
	t.Parallel()
	for _, sharing := range []string{"declared", "unknown", "mixed"} {
		t.Run(sharing, func(t *testing.T) {
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }}
			f := newNativeFixture(t, openTestService(t, cfg), "", "shared-provider")
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			runners := make([]runnerFixture, 2)
			for i := range runners {
				runners[i] = prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
				runners[i].enroll(t)
				report := capacityReport(now)
				report.AccountAlias = fmt.Sprintf("local-%d", i)
				if sharing == "unknown" || sharing == "mixed" && i == 1 {
					report.SharedAccountAlias = ""
				}
				publishCapacity(t, f, runners[i], report)
			}
			type result struct {
				status, runner int
				lease          tracker.NativeLease
			}
			outcomes := make(chan result, 8)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range 8 {
				issue := f.create(t, fmt.Sprintf("work-%d", i))
				wg.Go(func() {
					<-start
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runners[i%2].redemption.Credential, providerClaim(runners[i%2], issue, fmt.Sprintf("session-%d", i)))
					outcome := result{status: response.Code, runner: i % 2}
					if response.Code == http.StatusOK {
						decodeHubResponse(t, response, &outcome.lease)
					}
					outcomes <- outcome
				})
			}
			close(start)
			wg.Wait()
			close(outcomes)
			var winner result
			count := 0
			for outcome := range outcomes {
				if outcome.status == http.StatusOK {
					count++
					winner = outcome
				} else if outcome.status != http.StatusConflict {
					t.Fatalf("status = %d", outcome.status)
				}
			}
			if count != 1 {
				t.Fatalf("concurrent reservations = %d, want 1", count)
			}
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = openTestService(t, cfg)
			response := performHubAPIRequest(t, f.service, http.MethodGet, runners[0].base+"/runners", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var fleet []runnerauth.Runner
			decodeHubResponse(t, response, &fleet)
			for _, r := range fleet {
				if len(r.ProviderCapacity) != 1 || r.ProviderCapacity[0].Used != 1 {
					t.Fatalf("restart capacity = %+v", r.ProviderCapacity)
				}
			}
			blockedIssue := f.create(t, "blocked")
			claim := providerClaim(runners[1-winner.runner], blockedIssue, "blocked")
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runners[1-winner.runner].redemption.Credential, claim), http.StatusConflict)
			now = now.Add(90 * time.Second)
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runners[1-winner.runner].redemption.Credential, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var successor tracker.NativeLease
			decodeHubResponse(t, response, &successor)
			if successor.FencingToken <= winner.lease.FencingToken {
				t.Fatal("recovery reused fencing token")
			}
			path := f.base + "/leases/" + string(winner.lease.ID) + "/release"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runners[winner.runner].redemption.Credential, tracker.NativeLeaseMutation{FencingToken: winner.lease.FencingToken, Reason: "released"}), http.StatusConflict)
			path = f.base + "/leases/" + string(successor.ID) + "/release"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runners[1-winner.runner].redemption.Credential, tracker.NativeLeaseMutation{FencingToken: successor.FencingToken, Reason: "cancelled"}), http.StatusNoContent)
			claim.SessionID = "after-cancel"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runners[1-winner.runner].redemption.Credential, claim), http.StatusOK)
		})
	}
}

func TestProviderStartRevalidation(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"exhausted", "model", "account", "active"} {
		t.Run(change, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "revalidation")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			report := capacityReport(f.service.config.now())
			publishCapacity(t, f, r, report)
			issue := f.create(t, "work")
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issue, "session"))
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			event := nativeStartedEvent(lease)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			if change == "active" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, event), http.StatusOK)
			}
			switch change {
			case "exhausted", "active":
				report.Availability = "exhausted"
			case "model":
				event.Data.Identity.Model = "expensive"
			case "account":
				report.AccountAlias = "changed"
			}
			publishCapacity(t, f, r, report)
			if change == "active" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint", 2
				event.Data.Handoff = nativeTestCheckpoint()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, event), http.StatusOK)
			} else {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, event), http.StatusConflict)
			}
		})
	}
}

func TestProviderQueueOrderAndSelectors(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "fairness")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	report := capacityReport(f.service.config.now())
	publishCapacity(t, f, r, report)
	issues := []tracker.NativeIssue{f.create(t, "unsupported urgent"), f.create(t, "compatible high"), f.create(t, "compatible normal")}
	claim := providerClaim(r, issues[0], "fair")
	claim.WorkItemID = ""
	claim.ProviderCandidates = nil
	for i := len(issues) - 1; i >= 0; i-- {
		issue := issues[i]
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = ? WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", i, issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		candidate := providerClaim(r, issue, "").ProviderCandidates[0]
		if i == 0 {
			candidate.Requirement.Model = "unsupported"
		}
		claim.ProviderCandidates = append(claim.ProviderCandidates, candidate)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", r.redemption.Credential, tracker.NativeCapacityPreview{NativeClaim: claim})
	requireNativeStatus(t, response, http.StatusOK)
	var page tracker.NativeCapacityPage
	decodeHubResponse(t, response, &page)
	if len(page.Items) != 3 || page.Items[0].WorkItemID != issues[0].WorkItemID || page.Items[1].WorkItemID != issues[1].WorkItemID {
		t.Fatalf("preview reordered candidates: %+v", page)
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.WorkItemID != issues[1].WorkItemID {
		t.Fatal("capacity changed existing priority order")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	descriptor := hubTestPolicy()
	descriptor.Requirements.MachineID = string(runnerauth.NewBinding().MachineID)
	descriptor = descriptor.WithID()
	response = performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, map[string]any{"expected_policy_id": hubTestPolicy().ID, "policy": descriptor})
	requireNativeStatus(t, response, http.StatusOK)
	claim.PolicyID, claim.SessionID = descriptor.ID, "wrong-host"
	for _, path := range []string{"/claims", "/claims/preview"} {
		response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+path, r.redemption.Credential, claim)
		requireNativeStatus(t, response, http.StatusConflict)
		var failure nativeError
		decodeHubResponse(t, response, &failure)
		if failure.Code != "selector_no_match" {
			t.Fatalf("fixed host lost authority: %s", failure.Code)
		}
	}
}

func TestProviderOlderReportsCannotRestoreQuota(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "observations")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	report := capacityReport(f.service.config.now().Add(-time.Second))
	report.Availability = "exhausted"
	publishCapacity(t, f, r, report)
	for _, age := range []time.Duration{0, -time.Second, time.Hour} {
		stale := report
		stale.ObservedAt = stale.ObservedAt.Add(age)
		stale.Availability = "available"
		publishCapacity(t, f, r, stale)
		stored, err := readProviderReports(t.Context(), f.service.database.db, r.binding.RunnerID)
		if err != nil || len(stored) != 1 || stored[0].Availability != "exhausted" || !stored[0].ObservedAt.Equal(report.ObservedAt) {
			t.Fatalf("stale observation replaced exhaustion: %+v, %v", stored, err)
		}
	}
}

func TestProviderPoolIsolation(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "isolation")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.enroll(t)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	report := capacityReport(f.service.config.now())
	publishCapacity(t, f, r, report)
	issue := f.create(t, "reserved")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, issue, "reserved")), http.StatusOK)
	for _, test := range []struct {
		name, provider, shared string
		otherOrganization      bool
		used                   int
	}{
		{"same pool", "openai", "team", false, 1},
		{"explicit independent account", "openai", "independent", false, 0},
		{"unknown sharing", "openai", "", false, 1},
		{"other provider", "anthropic", "team", false, 0},
		{"other organization", "openai", "team", true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			other := report
			other.Provider, other.SharedAccountAlias, other.MaxConcurrent = test.provider, test.shared, 3
			organization := f.project.OrganizationID
			if test.otherOrganization {
				organization = "org_other"
			}
			view, err := providerView(t.Context(), f.service.database.db, organization, other, f.service.config.now())
			if err != nil || view.Used != test.used {
				t.Fatalf("view = %+v, %v", view, err)
			}
			if test.used != 0 && view.MaxConcurrent != 1 {
				t.Fatal("did not preserve the conservative shared bound")
			}
		})
	}
}
