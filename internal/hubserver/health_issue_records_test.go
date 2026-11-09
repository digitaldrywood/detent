package hubserver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresHealthIssuesPublishResolveAndRecur(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	now := service.config.now().UTC().Truncate(time.Microsecond)
	allFixtures := []nativeFixture{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixtures := []nativeFixture{}
		findings := []healthFinding{}
		for _, name := range []string{"First", "Second"} {
			f := newNativeFixture(t, service, org, name)
			states := slices.Clone(f.project.States)
			states = append(states, tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo", "Cancelled"}}, tracker.NativeState{Name: "Cancelled", Terminal: true})
			setHealthProjectStates(t, f, states)
			fixtures = append(fixtures, f)
			allFixtures = append(allFixtures, f)
			findings = append(findings, healthFinding{Fingerprint: "runner:" + string(f.project.ID), Signal: "runner_stale", Class: "instance", Subject: healthSubject{Kind: "runner", ID: "runner_" + string(f.project.ID)}, Projects: []string{string(f.project.ID)}, Severity: "attention", Summary: "Runner heartbeat is stale", NextAction: "Restore the runner connection"})
		}
		ids := map[tracker.ProjectID]tracker.NativeWorkItemID{}
		humanCancelled := map[tracker.ProjectID]bool{}
		for _, step := range []struct {
			age   time.Duration
			clear bool
			state string
		}{{0, false, "Backlog"}, {time.Minute, false, "Backlog"}, {2 * time.Minute, true, "Backlog"}, {2 * time.Hour, true, "Cancelled"}, {3 * time.Hour, false, "Backlog"}} {
			active := findings
			if step.clear {
				active = nil
			}
			if err := service.publishHealthEvaluation(t.Context(), org, now.Add(step.age), active); err != nil {
				t.Fatal(err)
			}
			for range fixtures {
				worked, err := service.maintainHealthIssue(t.Context(), org, now.Add(step.age))
				if err != nil {
					t.Fatalf("step=%s: %v", step.age, err)
				}
				if step.age != time.Minute && !worked {
					t.Fatalf("step=%s work missing", step.age)
				}
			}
			for _, f := range fixtures {
				response := performHubAPIRequest(t, service, http.MethodGet, f.base+"/health/findings?state=all", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page healthFindingsPage
				decodeHubResponse(t, response, &page)
				index := slices.IndexFunc(page.Items, func(finding healthFinding) bool { return finding.ResolvedAt == nil })
				if step.clear {
					index = 0
				}
				if index < 0 || len(page.Items[index].Issues) != 1 {
					t.Fatalf("step=%s page=%+v", step.age, page)
				}
				id := page.Items[index].Issues[0].WorkItemID
				if step.age == 0 {
					ids[f.project.ID] = id
				} else if (id == ids[f.project.ID]) != (step.age < 3*time.Hour || humanCancelled[f.project.ID]) {
					t.Fatalf("step=%s issue identity=%s original=%s", step.age, id, ids[f.project.ID])
				}
				issueResponse := performHubAPIRequest(t, service, http.MethodGet, f.base+"/work-items/"+string(id), f.token, nil)
				requireNativeStatus(t, issueResponse, http.StatusOK)
				var issue tracker.NativeIssue
				decodeHubResponse(t, issueResponse, &issue)
				state := step.state
				if humanCancelled[f.project.ID] {
					state = "Cancelled"
				}
				if issue.State != state || issue.Actor != *nativeIntegrationActor("health_detector") {
					t.Fatalf("step=%s issue=%+v", step.age, issue)
				}
				if step.age == 2*time.Minute && f.project.ID == fixtures[0].project.ID {
					response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(id)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "human-health-cancellation"}, ExpectedRevision: issue.Revision, State: "Cancelled", Reason: "user_requested"})
					requireNativeStatus(t, response, http.StatusOK)
					humanCancelled[f.project.ID] = true
				}
			}
		}
	}
	cfg := service.config
	cfg.Hosted = &HostedConfig{OrganizationID: "org_first", WorkOSOrganizationID: "provider_org_first", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, f := range allFixtures {
		page, err := reopened.readHealthFindings(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, "all", "", "", 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 2 || len(page.Items[0].Issues) != 1 || len(page.Items[1].Issues) != 1 {
			t.Fatalf("reopened project=%s findings=%+v", f.project.ID, page)
		}
	}
}

type healthPublicationConcurrentRepository struct {
	tenantstore.Repository
	arrivals chan struct{}
	release  chan struct{}
	calls    atomic.Int32
}

type healthPublicationConcurrentWriter struct {
	tenantstore.Writer
	healthstore.IssueWriter
	searchstore.DuplicateSourceWriter
	gate *healthPublicationConcurrentRepository
}

func (r *healthPublicationConcurrentRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		return write(healthPublicationConcurrentWriter{Writer: w, IssueWriter: w.(healthstore.IssueWriter), DuplicateSourceWriter: w.(searchstore.DuplicateSourceWriter), gate: r})
	})
}

func (w healthPublicationConcurrentWriter) LockWorkItemProject(ctx context.Context, project tracker.ProjectID) error {
	if w.gate.calls.Add(1) <= 2 {
		w.gate.arrivals <- struct{}{}
		select {
		case <-w.gate.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return w.Writer.LockWorkItemProject(ctx, project)
}

func TestPostgresHealthConcurrentFindingsShareIntakeOwner(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(organization), func(t *testing.T) {
			f := newNativeFixture(t, service, organization, "Concurrent health")
			addHealthBacklog(t, f)
			now := service.config.now().UTC().Truncate(time.Microsecond)
			findings := []healthFinding{}
			for _, id := range []string{"first", "second"} {
				findings = append(findings, healthFinding{Fingerprint: id, Signal: "retry_storm", Class: "instance", Subject: healthSubject{Kind: "runner", ID: id}, Projects: []string{string(f.project.ID)}, Severity: "attention", Summary: "Repeated instance failure", NextAction: "Repair the instance failure", Evidence: healthEvidence{Signatures: []string{"example_signature"}}})
			}
			if err := service.publishHealthEvaluation(t.Context(), organization, now, findings); err != nil {
				t.Fatal(err)
			}
			page, err := service.readHealthFindings(t.Context(), nativeScope{organization: organization, project: f.project.ID}, "all", "", "", 100)
			if err != nil || len(page.Items) != 2 {
				t.Fatalf("findings=%+v err=%v", page, err)
			}
			original := service.config.TenantRepository
			gate := &healthPublicationConcurrentRepository{Repository: original, arrivals: make(chan struct{}, 2), release: make(chan struct{})}
			service.config.TenantRepository = gate
			ctx, cancel := context.WithCancel(t.Context())
			var workers sync.WaitGroup
			var release sync.Once
			defer func() {
				cancel()
				release.Do(func() { close(gate.release) })
				workers.Wait()
				service.config.TenantRepository = original
			}()
			results := make(chan error, 2)
			for _, finding := range page.Items {
				var revision int64
				if err := service.database.postgres.ReadHealth(ctx, organization, corestore.Background, func(r postgres.HealthReader) error {
					row, err := r.HealthRecord(ctx, postgres.HealthRecordInput{OrganizationID: organization, ID: finding.ID})
					revision = row.Revision
					return err
				}); err != nil {
					t.Fatal(err)
				}
				scope := healthIssueScope(organization, f.project.ID)
				scope.credential = apiCredential{ID: "health_detector", Scope: apiScopeAdmin}
				scope.healthPublication = &healthIssuePublication{work: healthstore.IssueWork{OrganizationID: organization, FindingID: finding.ID, ProjectID: f.project.ID, SourceRevision: revision}, openedAt: finding.OpenedAt}
				occurrence := healthIssueFindings(finding)[0]
				workers.Go(func() { results <- service.publishHealthIssueOccurrence(ctx, scope, occurrence) })
			}
			for range 2 {
				select {
				case <-gate.arrivals:
				case err := <-results:
					t.Fatalf("publication completed before concurrent plans: %v", err)
				}
			}
			release.Do(func() { close(gate.release) })
			for range 2 {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			page, err = service.readHealthFindings(ctx, nativeScope{organization: organization, project: f.project.ID}, "all", "", "", 100)
			if err != nil || len(page.Items) != 2 || len(page.Items[0].Issues) != 1 || len(page.Items[1].Issues) != 1 || page.Items[0].Issues[0].WorkItemID != page.Items[1].Issues[0].WorkItemID {
				t.Fatalf("concurrent finding owners=%+v err=%v", page, err)
			}
		})
	}
}

func TestPostgresHealthDoneIssuesRecurWithoutNewEvidence(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		f := newNativeFixture(t, service, organization, "Completed health")
		states := append(slices.Clone(f.project.States), tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo", "Done"}})
		setHealthProjectStates(t, f, states)
		now := service.config.now().UTC().Truncate(time.Microsecond)
		findings := evaluateHealth(now, healthSnapshot{Runners: []healthRunner{{ID: "runner_first", Projects: []string{string(f.project.ID)}, CreatedAt: now.Add(-time.Hour)}, {ID: "runner_second", Projects: []string{string(f.project.ID)}, CreatedAt: now.Add(-time.Hour)}}})
		if err := service.publishHealthEvaluation(t.Context(), organization, now, findings); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := service.maintainHealthIssue(t.Context(), organization, now); err != nil {
				t.Fatal(err)
			}
		}
		page, err := service.readHealthFindings(t.Context(), nativeScope{organization: organization, project: f.project.ID}, "all", "", "", 100)
		if err != nil || len(page.Items) != 2 {
			t.Fatalf("findings=%+v err=%v", page, err)
		}
		previous := map[string]tracker.NativeWorkItemID{}
		for _, finding := range page.Items {
			if len(finding.Issues) != 1 {
				t.Fatalf("finding=%+v", finding)
			}
			id := finding.Issues[0].WorkItemID
			previous[finding.ID] = id
			response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(id)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "finish-health-" + finding.ID}, ExpectedRevision: 1, State: "Done", Reason: "user_requested"})
			requireNativeStatus(t, response, http.StatusOK)
		}
		for range 2 {
			if _, err := service.maintainHealthIssue(t.Context(), organization, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		page, err = service.readHealthFindings(t.Context(), nativeScope{organization: organization, project: f.project.ID}, "all", "", "", 100)
		if err != nil || len(page.Items) != 2 {
			t.Fatalf("recurrence=%+v err=%v", page, err)
		}
		for _, finding := range page.Items {
			if len(finding.Issues) != 2 || !slices.ContainsFunc(finding.Issues, func(link healthFindingIssue) bool { return link.WorkItemID == previous[finding.ID] }) {
				t.Fatalf("done recurrence lost original or new issue: %+v", finding)
			}
		}
	}
}

func TestPostgresHealthPublicationRejectsForgedAuthority(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	first := newNativeFixture(t, service, "org_first", "First")
	second := newNativeFixture(t, service, "org_first_second", "Second")
	work := healthstore.IssueWork{OrganizationID: first.project.OrganizationID, ProjectID: first.project.ID, FindingID: "missing", SourceRevision: 1}
	for _, scope := range []nativeScope{
		{organization: second.project.OrganizationID, project: second.project.ID, sourceActor: nativeIntegrationActor("health_detector"), healthPublication: &healthIssuePublication{work: work}},
		{organization: first.project.OrganizationID, project: first.project.ID, sourceActor: nativeIntegrationActor("example"), healthPublication: &healthIssuePublication{work: work}},
	} {
		err := service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Background, func(r tenantstore.Reader) error { return service.authorizeAccessScope(t.Context(), r, scope, true) })
		if !errors.Is(err, accessstore.ErrDenied) {
			t.Fatalf("forged authority=%v", err)
		}
	}
}
