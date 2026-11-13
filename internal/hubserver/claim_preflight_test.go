package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestClaimSelectionStaysOffTheWriter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	open := func(t *testing.T) (nativeFixture, *sql.DB, []tracker.NativeIssue) {
		t.Helper()
		repository, admin := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
			config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, claimPreflightTrace{})
		})
		service := openLocalPostgresHubTestService(t, repository, Config{now: func() time.Time { return now }, RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1")})
		for _, organization := range []nativeOrganization{{ID: "org_first_second", Name: "Second", Slug: "second"}, {ID: "org_first", Name: "First", Slug: "first"}} {
			seedPostgresHubAdmin(t, repository, service, organization)
			key := bytes.Repeat([]byte{0x5a}, 32)
			envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(organization.ID, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization.ID, ID: hubapi.NewID("hub"), CursorKey: key}); err != nil {
					return err
				}
				return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization.ID, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
		}
		service.localOrganization, service.database.hostedOrganization, service.requestMetrics.Organization = "org_first", "org_first", "org_first"
		f := newNativeFixture(t, service, "org_first", "claim preflight")
		return f, admin, []tracker.NativeIssue{f.create(t, "first"), f.create(t, "second")}
	}
	type claimRunner struct {
		scope   hubscope.NativeScope
		request tracker.NativeClaim
	}
	newRunner := func(t *testing.T, f nativeFixture) claimRunner {
		t.Helper()
		runner := prepareRunner(t, f, runnerauth.Claim)
		runner.redemption.Capacity = 1
		runner.enroll(t)
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			return w.UpdateRunnerCapacityConfig(t.Context(), runner.binding.RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(runner.redemption.Credential), "", "")
		if err != nil {
			t.Fatalf("runner authentication status=%d: %v", status, err)
		}
		return claimRunner{scope: hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}, request: tracker.NativeClaim{PolicyID: hubTestPolicy().ID, MachineID: runner.binding.MachineID, TTLSeconds: 60, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}}
	}
	for _, name := range []string{"a refusal with no candidate never takes the writer", "concurrent claims never oversubscribe one host slot", "a selected candidate taken before commit moves to the next one"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, admin, issues := open(t)
			claim := func(ctx context.Context, runner claimRunner, session string, item tracker.NativeWorkItemID) (string, error) {
				request := runner.request
				request.SessionID, request.WorkItemID = session, item
				lease, err := f.service.claimNativeIssueTenant(ctx, runner.scope, request, time.Minute)
				return lease.LeaseID, err
			}
			switch name {
			case "a refusal with no candidate never takes the writer":
				first, second, third := newRunner(t, f), newRunner(t, f), newRunner(t, f)
				positive := &claimPreflightProbe{attempted: make(chan struct{})}
				positiveContext := context.WithValue(t.Context(), claimPreflightProbeKey{}, positive)
				if _, err := claim(positiveContext, first, "session-a", issues[0].WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := claim(positiveContext, second, "session-b", issues[1].WorkItemID); err != nil {
					t.Fatal(err)
				}
				if positive.writers.Load() == 0 || positive.mutations.Load() == 0 {
					t.Fatal("claim trace did not observe actual successful claim writes")
				}
				held, release, writerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() {
					writerDone <- f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						if err := w.LockAccessOrganization(t.Context()); err != nil {
							return err
						}
						close(held)
						<-release
						return nil
					})
				}()
				select {
				case <-held:
				case err := <-writerDone:
					t.Fatalf("hold organization writer: %v", err)
				}
				var releaseOnce sync.Once
				unlock := func() {
					releaseOnce.Do(func() {
						close(release)
						if err := <-writerDone; err != nil {
							t.Error(err)
						}
					})
				}
				t.Cleanup(unlock)
				probe := &claimPreflightProbe{attempted: make(chan struct{})}
				ctx := context.WithValue(t.Context(), claimPreflightProbeKey{}, probe)
				done := make(chan error, 1)
				go func() { _, err := claim(ctx, third, "session-c", ""); done <- err }()
				var claimErr error
				select {
				case claimErr = <-done:
					unlock()
				case <-probe.attempted:
					unlock()
					claimErr = <-done
					t.Fatalf("refused claim entered the writer: %v", claimErr)
				case <-testenv.WaitContext(t, t.Context()).Done():
					unlock()
					claimErr = <-done
					t.Fatalf("refused claim waited for the held writer: %v", claimErr)
				}
				if !errors.Is(claimErr, ErrNoClaimableWork) {
					t.Fatalf("refused claim error = %v, want no claimable work", claimErr)
				}
				if probe.queries.Load() == 0 || probe.writers.Load() != 0 || probe.mutations.Load() != 0 {
					t.Fatalf("refused claim queries=%d writers=%d mutations=%d, want queries and zero writes", probe.queries.Load(), probe.writers.Load(), probe.mutations.Load())
				}
			case "concurrent claims never oversubscribe one host slot":
				runner := newRunner(t, f)
				const contenders = 8
				var wait sync.WaitGroup
				errs := make(chan error, contenders)
				for index := range contenders {
					wait.Go(func() { _, err := claim(t.Context(), runner, fmt.Sprintf("session-%d", index), ""); errs <- err })
				}
				wait.Wait()
				close(errs)
				won := 0
				for err := range errs {
					if err == nil {
						won++
					} else if !errors.Is(err, ErrNoClaimableWork) {
						t.Fatalf("claim error = %v", err)
					}
				}
				var active int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM leases WHERE organization_id=$1 AND machine_id=$2 AND released_at IS NULL", f.project.OrganizationID, runner.request.MachineID).Scan(&active); err != nil {
					t.Fatal(err)
				}
				if won != 1 || active != 1 {
					t.Fatalf("winners = %d active leases = %d, want 1 and 1 for a capacity-1 host", won, active)
				}
			case "a selected candidate taken before commit moves to the next one":
				first, second := newRunner(t, f), newRunner(t, f)
				request := second.request
				request.SessionID, request.WorkItemID = "session-b", issues[0].WorkItemID
				plan, err := f.service.readNativeClaimPlan(t.Context(), second.scope, request, now)
				if err != nil {
					t.Fatal(err)
				}
				taken, err := claim(t.Context(), first, "session-a", issues[0].WorkItemID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.claimNativeIssueTenantWithPlan(t.Context(), second.scope, request, time.Minute, now, plan, nil); !errors.Is(err, ErrNoClaimableWork) {
					t.Fatalf("stale selected plan error = %v, want no claimable work", err)
				}
				selected, err := claim(t.Context(), second, "session-b", "")
				if err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					for _, pair := range []struct {
						lease string
						item  tracker.NativeWorkItemID
					}{{taken, issues[0].WorkItemID}, {selected, issues[1].WorkItemID}} {
						lease, err := r.Lease(t.Context(), pair.lease)
						if err != nil {
							return err
						}
						item, err := r.WorkItem(t.Context(), f.project.ID, pair.item)
						if err != nil {
							return err
						}
						if lease.IssueID != item.ID {
							return fmt.Errorf("claim after stale plan lease=%s issue=%d, want item %s (%d)", pair.lease, lease.IssueID, pair.item, item.ID)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestClaimRetryAfterCommittedDecisionUsesFreshEventIDs(t *testing.T) {
	f := newNativeFixture(t, nil, "", "claim-retry-event-ids")
	runner := prepareRunner(t, f, runnerauth.Claim)
	runner.enroll(t)
	org := f.project.OrganizationID
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.UpdateRunnerCapacityConfig(t.Context(), runner.binding.RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: f.service.config.now()})
	}); err != nil {
		t.Fatal(err)
	}
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(runner.redemption.Credential), "", "")
	if err != nil {
		t.Fatalf("runner authentication status=%d error=%v", status, err)
	}
	scope := hubscope.NativeScope{Organization: org, Project: f.project.ID, Credential: credential}
	first, second, selected := f.create(t, "first decision"), f.create(t, "second decision"), f.create(t, "selected candidate")
	requestFor := func(issue tracker.NativeIssue) tracker.NativeClaim {
		return tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: runner.binding.MachineID, SessionID: "retry-" + string(issue.WorkItemID), TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
	}
	var firstLeaseID string
	for pass, issues := range [][]tracker.NativeIssue{{first}, {first, second}} {
		for _, issue := range issues {
			request := requestFor(issue)
			lease, err := f.service.claimNativeIssueTenant(t.Context(), scope, request, 5*time.Minute)
			if err != nil {
				t.Fatalf("pass %d claim: %v", pass, err)
			}
			if issue.WorkItemID == first.WorkItemID {
				if pass == 0 {
					firstLeaseID = lease.LeaseID
				} else if lease.LeaseID != firstLeaseID {
					t.Fatalf("replayed lease = %s, want %s", lease.LeaseID, firstLeaseID)
				}
			}
		}
		request := requestFor(selected)
		now := f.service.config.now()
		stale, err := f.service.readNativeClaimPlan(t.Context(), scope, request, now)
		if err != nil {
			t.Fatal(err)
		}
		stale.candidate.Revision = -1
		if _, err := f.service.claimNativeIssueTenantWithPlan(t.Context(), scope, request, 5*time.Minute, now, stale, nil); !errors.Is(err, ErrNoClaimableWork) {
			t.Fatalf("pass %d: %v, want committed decisions before stale plan refusal", pass, err)
		}
	}
	events := 0
	ids := map[string]bool{}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		for _, issue := range []tracker.NativeIssue{first, second, selected} {
			rows, err := r.Events(t.Context(), f.project.ID, issue.WorkItemID, 0, 100)
			if err != nil {
				return err
			}
			itemEvents := 0
			for _, row := range rows {
				if row.Type != "scheduler.decision" {
					continue
				}
				if issue.WorkItemID == selected.WorkItemID {
					t.Error("stale candidate committed a scheduling decision")
				}
				events++
				itemEvents++
				ids[row.ID] = true
			}
			want := 1
			if issue.WorkItemID == selected.WorkItemID {
				want = 0
			}
			if itemEvents != want {
				t.Errorf("item %s decisions = %d, want %d", issue.WorkItemID, itemEvents, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if events != 2 || len(ids) != 2 {
		t.Fatalf("scheduler decisions=%d distinct ids=%d, want 2 committed events with distinct ids", events, len(ids))
	}
}

type claimPreflightProbeKey struct{}
type claimPreflightProbe struct {
	queries   atomic.Int64
	writers   atomic.Int64
	mutations atomic.Int64
	attempted chan struct{}
	once      sync.Once
}
type claimPreflightTrace struct{}

func (claimPreflightTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	traceClaimPreflight(ctx, data.SQL)
	return ctx
}
func (claimPreflightTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (claimPreflightTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		traceClaimPreflight(ctx, query.SQL)
	}
	return ctx
}
func (claimPreflightTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (claimPreflightTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData)     {}
func traceClaimPreflight(ctx context.Context, query string) {
	probe, ok := ctx.Value(claimPreflightProbeKey{}).(*claimPreflightProbe)
	if !ok {
		return
	}
	probe.queries.Add(1)
	statement := strings.ToLower(strings.TrimSpace(query))
	for strings.HasPrefix(statement, "--") {
		_, rest, found := strings.Cut(statement, "\n")
		if !found {
			return
		}
		statement = strings.TrimSpace(rest)
	}
	if strings.HasPrefix(statement, "begin") && !strings.Contains(statement, "read only") {
		probe.writers.Add(1)
		probe.once.Do(func() { close(probe.attempted) })
	}
	for _, prefix := range []string{"insert ", "update ", "delete ", "merge ", "truncate "} {
		if strings.HasPrefix(statement, prefix) {
			probe.mutations.Add(1)
			break
		}
	}
}
