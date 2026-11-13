package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type claimCandidateProbeKey struct{}

type claimCandidateProbe struct {
	wire       heartbeatWire
	once       sync.Once
	writes     int
	reads      nativePreviewReadCounts
	candidates []claimstore.Candidate
}

type claimCandidateRepository struct {
	*postgres.Repository
	ready   chan struct{}
	release chan struct{}
}

func (r *claimCandidateRepository) ReadTenantSnapshot(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	probe, ok := ctx.Value(claimCandidateProbeKey{}).(*claimCandidateProbe)
	if !ok {
		return r.Repository.ReadTenantSnapshot(ctx, organization, workload, read)
	}
	probe.reads.readTenant++
	return r.Repository.ReadTenantSnapshot(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(claimCandidateReader{nativePreviewCountingReader: nativePreviewCountingReader{Reader: reader, counts: &probe.reads}, probe: probe})
	})
}

type claimCandidateReader struct {
	nativePreviewCountingReader
	probe *claimCandidateProbe
}

func (r claimCandidateReader) CandidatePage(ctx context.Context, query claimstore.CandidateQuery) (claimstore.CandidatePage, error) {
	page, err := r.nativePreviewCountingReader.CandidatePage(ctx, query)
	r.probe.candidates = append(r.probe.candidates, page.Items...)
	return page, err
}

func (r *claimCandidateRepository) WriteRunnerCommand(ctx context.Context, organization tracker.OrganizationID, credential, runner string, write func(context.Context, tenantstore.Writer) error) error {
	if probe, ok := ctx.Value(claimCandidateProbeKey{}).(*claimCandidateProbe); ok {
		probe.once.Do(func() {
			r.ready <- struct{}{}
			select {
			case <-r.release:
			case <-ctx.Done():
			}
		})
		probe.writes++
	}
	return r.Repository.WriteRunnerCommand(ctx, organization, credential, runner, write)
}

func TestClaimNextCandidateConcurrent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                          string
		runners, items, organizations int
	}{
		{"uncontended", 1, 2, 1},
		{"second candidate", 2, 2, 1},
		{"more items", 2, 8, 1},
		{"exhausted page", 16, 8, 1},
		{"two organizations", 16, 8, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, admin := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
				config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
					conn, err := new(net.Dialer).DialContext(ctx, network, address)
					if err != nil {
						return nil, err
					}
					return &heartbeatConn{Conn: conn, wire: new(heartbeatWire), startup: true}, nil
				}
				config.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
					stream, ok := conn.PgConn().Conn().(*heartbeatConn)
					if !ok {
						return false, fmt.Errorf("missing claim wire wrapper: %T", conn.PgConn().Conn())
					}
					stream.wire = new(heartbeatWire)
					if probe, ok := ctx.Value(claimCandidateProbeKey{}).(*claimCandidateProbe); ok {
						stream.wire = &probe.wire
					}
					return true, nil
				}
			})
			now := time.Now().UTC().Truncate(time.Microsecond)
			service := openLocalPostgresHubTestService(t, repository, Config{Clock: func() time.Time { return now }, RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1")})
			type claimer struct {
				scope   hubscope.NativeScope
				request tracker.NativeClaim
				probe   claimCandidateProbe
			}
			workers := make([]claimer, test.runners*test.organizations)
			projects := make([]tracker.NativeProject, test.organizations)
			for organizationIndex := range test.organizations {
				organization := nativeOrganization{ID: tracker.OrganizationID(fmt.Sprintf("org_claim_%d", organizationIndex)), Name: fmt.Sprintf("Organization %d", organizationIndex), Slug: fmt.Sprintf("org-claim-%d", organizationIndex)}
				seedPostgresHubAdmin(t, repository, service, organization)
				service.localOrganization, service.database.hostedOrganization, service.requestMetrics.Organization = string(organization.ID), organization.ID, organization.ID
				f := newNativeFixture(t, service, organization.ID, "claim candidates")
				projects[organizationIndex] = f.project
				approveHubTestPolicy(t, service, f.base+"/policy", hubTestPolicy())
				requests := make([]tracker.CreateIssue, test.items)
				for index := range requests {
					requests[index] = tracker.CreateIssue{Title: fmt.Sprintf("ready-%d", index), Body: issueContractTestSections, State: "Todo"}
				}
				publish, _ := prepareDispatchIssuesFixture(t, f, requests)
				publish()
				for index := range test.runners {
					runner := prepareRunner(t, f, runnerauth.Claim)
					runner.redemption.Capacity = 1
					runner.enroll(t)
					if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
						return w.UpdateRunnerCapacityConfig(t.Context(), runner.binding.RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: now})
					}); err != nil {
						t.Fatal(err)
					}
					credential, status, err := service.authenticateTenantAPIHash(t.Context(), organization.ID, apikey.HashToken(runner.redemption.Credential), "", "")
					if err != nil || status != http.StatusOK {
						t.Fatalf("authenticate runner: status=%d error=%v", status, err)
					}
					workers[organizationIndex*test.runners+index] = claimer{
						scope:   hubscope.NativeScope{Organization: organization.ID, Project: f.project.ID, Credential: credential},
						request: tracker.NativeClaim{PolicyID: hubTestPolicy().ID, MachineID: runner.binding.MachineID, SessionID: fmt.Sprintf("session-%d-%d", organizationIndex, index), TTLSeconds: 60, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, WorkflowStates: []string{"Todo"}},
					}
				}
			}
			gated := &claimCandidateRepository{Repository: repository, ready: make(chan struct{}, len(workers)), release: make(chan struct{})}
			service.config.TenantRepository = gated
			var release sync.Once
			ctx, cancel := context.WithCancel(t.Context())
			var joined sync.WaitGroup
			defer func() { cancel(); release.Do(func() { close(gated.release) }); joined.Wait() }()
			type result struct {
				index int
				lease leasestore.Lease
				err   error
			}
			results := make(chan result, len(workers))
			for index := range workers {
				joined.Go(func() {
					worker := &workers[index]
					requestContext := context.WithValue(ctx, claimCandidateProbeKey{}, &worker.probe)
					lease, err := service.claimNativeIssueTenant(requestContext, worker.scope, worker.request, time.Minute)
					results <- result{index: index, lease: lease, err: err}
				})
			}
			for range workers {
				select {
				case <-gated.ready:
				case result := <-results:
					t.Fatalf("claim ended before write barrier: %v", result.err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			release.Do(func() { close(gated.release) })
			grants := make([]int, test.organizations)
			leases, fences, items := map[string]bool{}, map[int64]bool{}, map[int64]bool{}
			for range workers {
				result := <-results
				worker := &workers[result.index]
				worker.probe.wire.Lock()
				t.Logf("organization=%s runner=%d granted=%t writes=%d statements=%d exchanges=%d", worker.scope.Organization, result.index, result.err == nil, worker.probe.writes, worker.probe.wire.statements, worker.probe.wire.exchanges)
				worker.probe.wire.Unlock()
				if worker.probe.reads.readTenant != 1 || worker.probe.reads.candidatePage != 1 || len(worker.probe.candidates) != test.items {
					t.Errorf("claim selection escaped one page: reads=%+v candidates=%d", worker.probe.reads, len(worker.probe.candidates))
				}
				if result.err != nil {
					if !errors.Is(result.err, ErrNoClaimableWork) {
						t.Errorf("claim runner %d: %v", result.index, result.err)
					}
					continue
				}
				lease := result.lease
				position := worker.probe.writes - 1
				if position < 0 || position >= len(worker.probe.candidates) || worker.probe.candidates[position].ID != lease.IssueID {
					t.Errorf("claim did not follow selected candidate order: writes=%d lease=%+v", worker.probe.writes, lease)
				}
				if lease.OrganizationID != worker.scope.Organization || lease.ProjectID == nil || *lease.ProjectID != worker.scope.Project || lease.LeaseID == "" || lease.FencingToken <= 0 || leases[lease.LeaseID] || fences[lease.FencingToken] || items[lease.IssueID] {
					t.Errorf("duplicate or invalid claim: %+v", lease)
				}
				leases[lease.LeaseID], fences[lease.FencingToken], items[lease.IssueID] = true, true, true
				grants[result.index/test.runners]++
			}
			joined.Wait()
			for index, project := range projects {
				var held, uniqueItems, uniqueFences int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*),count(DISTINCT issue_id),count(DISTINCT fencing_token) FROM leases WHERE organization_id=$1 AND project_id=$2 AND released_at IS NULL", project.OrganizationID, project.ID).Scan(&held, &uniqueItems, &uniqueFences); err != nil {
					t.Fatal(err)
				}
				want := min(test.runners, test.items)
				if grants[index] != want || held != want || uniqueItems != want || uniqueFences != want {
					t.Errorf("organization=%s grants=%d leases=%d items=%d fences=%d want=%d", project.OrganizationID, grants[index], held, uniqueItems, uniqueFences, want)
				}
			}
		})
	}
}
