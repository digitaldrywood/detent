package hubserver

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthContractQueries struct {
	tenantstore.Repository
	tenantstore.SnapshotRepository
	issues   int
	policies int
}

func (q *healthContractQueries) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return q.Repository.ReadTenant(ctx, organization, workload, func(r tenantstore.Reader) error {
		return read(healthContractQueryReader{Reader: r, ObservationReader: r.(healthstore.ObservationReader), ScheduledReader: r.(healthstore.ScheduledReader), counts: q})
	})
}

type healthContractQueryReader struct {
	tenantstore.Reader
	healthstore.ObservationReader
	healthstore.ScheduledReader
	counts *healthContractQueries
}

func (r healthContractQueryReader) WorkItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	r.counts.issues++
	return r.Reader.WorkItem(ctx, project, item)
}

func (r healthContractQueryReader) ProjectPolicy(ctx context.Context, project tracker.ProjectID) (policystore.Revision, error) {
	r.counts.policies++
	return r.Reader.ProjectPolicy(ctx, project)
}

func seedHealthContractIssue(t *testing.T, f nativeFixture, issue tracker.NativeIssue) {
	t.Helper()
	fields, err := f.service.prepareWorkItemFields(t.Context(), issue.OrganizationID, workItemRecordPlan{issue: issue})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	version, err := eventstore.NewContent(f.service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.config.TenantRepository.WriteTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), issue.ProjectID, issue.WorkItemID)
		if err != nil {
			return err
		}
		row.BodyObjectKey, row.IssueContractObjectKey = &fields.Body, fields.Contract
		row.NativeUpdatedAt = issue.UpdatedAt
		if _, err = w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		_, err = w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHealthContractsReuseRevisions(t *testing.T) {
	t.Parallel()
	service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
	f := newNativeFixture(t, service, "org_first", "health-contracts")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-health-contracts")
	first, second := f.create(t, "first"), other.create(t, "second")
	now := time.Now().UTC().Truncate(time.Microsecond)
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	runner.enroll(t)
	admin := healthFixtureDatabase(t, f)
	if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=$1,created_at=$1 WHERE organization_id=$2 AND runner_id=$3`, now, f.project.OrganizationID, runner.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	appendTestHealthEvent(t, other, second, "workflow.transitioned", tracker.CollaborationData{Reason: "worker_progress", ReasonDetail: "permission_wait"}, now)
	if err := service.config.TenantRepository.WriteTenant(t.Context(), other.project.OrganizationID, corestore.Background, func(w tenantstore.Writer) error {
		_, err := w.(healthstore.ScheduledWriter).RecordHealthScheduledReport(t.Context(), healthstore.ScheduledReport{OrganizationID: other.project.OrganizationID, ProjectID: other.project.ID, RecordedAt: now.Add(-5*time.Hour - 29*time.Minute), ScheduledValidationReport: gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: "1", RunAttempt: "1", RunURL: "https://github.com/example-org/example-project/actions/runs/1/attempts/1", HeadSHA: strings.Repeat("a", 40), Conclusion: "success"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	originalBody := first.Body
	original := service.config.TenantRepository
	queries := &healthContractQueries{Repository: original, SnapshotRepository: original.(tenantstore.SnapshotRepository)}
	t.Cleanup(func() { service.config.TenantRepository = original })
	for _, test := range []struct {
		name      string
		change    func()
		loads     int
		contracts int
		heartbeat bool
		elapsed   bool
	}{
		{name: "first tick", loads: 2},
		{name: "unchanged next tick"},
		{name: "issue revision invalidates one project", change: func() {
			first.Body = "Incomplete contract"
			first.Revision++
			first.UpdatedAt = now
			seedHealthContractIssue(t, f, first)
		}, loads: 1, contracts: 1},
		{name: "unsatisfied result is reused", contracts: 1},
		{name: "policy revision invalidates one project", change: func() {
			changed := hubTestPolicy()
			changed.SourceRevision = strings.Repeat("b", 40)
			approveHubTestPolicy(t, f.service, f.base+"/policy", changed.WithID())
		}, loads: 1, contracts: 1},
		{name: "heartbeat advances without reevaluation", change: func() { now = now.Add(5 * time.Minute) }, contracts: 1, heartbeat: true},
		{name: "elapsed wait advances without reevaluation", change: func() { now = now.Add(26 * time.Minute) }, contracts: 1, heartbeat: true, elapsed: true},
		{name: "issue correction clears cached finding", change: func() {
			first.Body = originalBody
			first.Revision++
			first.UpdatedAt = now
			seedHealthContractIssue(t, f, first)
		}, loads: 1, heartbeat: true, elapsed: true},
		{name: "policy removal invalidates", change: func() {
			if _, err := admin.ExecContext(t.Context(), `DELETE FROM project_policies WHERE organization_id=$1 AND project_id=$2`, other.project.OrganizationID, other.project.ID); err != nil {
				t.Fatal(err)
			}
		}, loads: 1, heartbeat: true, elapsed: true},
		{name: "missing policy is reused", heartbeat: true, elapsed: true},
		{name: "policy restoration invalidates", change: func() {
			seedNativeFixturePolicy(t, f.service, string(other.project.OrganizationID)+"/"+string(other.project.ID))
		}, loads: 1, heartbeat: true, elapsed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service.config.TenantRepository = original
			if test.change != nil {
				test.change()
			}
			service.config.TenantRepository = queries
			queries.issues, queries.policies = 0, 0
			snapshot, err := service.readHealthSnapshotRecords(t.Context(), f.project.OrganizationID, now)
			service.config.TenantRepository = original
			if err != nil {
				t.Fatal(err)
			}
			if queries.issues != test.loads || queries.policies != 2 {
				t.Fatalf("issue loads=%d project policy reads=%d, want %d and 2", queries.issues, queries.policies, test.loads)
			}
			cache := &service.healthContracts
			cache.mu.Lock()
			entries, order, oldest := cache.entries, cache.order, cache.oldest
			cache.entries, cache.order, cache.oldest = nil, nil, 0
			cache.mu.Unlock()
			uncached, err := service.readHealthSnapshotRecords(t.Context(), f.project.OrganizationID, now)
			cache.mu.Lock()
			cache.entries, cache.order, cache.oldest = entries, order, oldest
			cache.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			findings, want := evaluateHealth(now, snapshot), evaluateHealth(now, uncached)
			sort := func(findings []healthFinding) {
				slices.SortFunc(findings, func(a, b healthFinding) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
			}
			sort(findings)
			sort(want)
			if !reflect.DeepEqual(findings, want) {
				t.Fatalf("cached findings=%+v, uncached=%+v", findings, want)
			}
			contracts, heartbeat, elapsed, scheduled := 0, false, false, false
			for _, finding := range findings {
				if finding.Evidence.Reason == "issue_contract_unsatisfied" {
					contracts++
					if finding.Subject.ID != string(first.WorkItemID) || !slices.Equal(finding.Projects, []string{string(f.project.ID)}) {
						t.Fatalf("contract finding escaped project: %+v", finding)
					}
				}
				heartbeat = heartbeat || finding.Signal == "runner_heartbeat_gap"
				elapsed = elapsed || finding.Signal == "unanswered_human_wait" && finding.Subject.ID == string(second.WorkItemID)
				scheduled = scheduled || finding.Signal == "dead_man" && finding.Subject.ID == string(other.project.ID) && finding.Evidence.Counts["scheduled_silent"] == 1
			}
			if contracts != test.contracts || heartbeat != test.heartbeat || elapsed != test.elapsed || scheduled != test.elapsed {
				t.Fatalf("contracts=%d heartbeat=%t elapsed=%t scheduled=%t, want %d %t %t %t", contracts, heartbeat, elapsed, scheduled, test.contracts, test.heartbeat, test.elapsed, test.elapsed)
			}
		})
	}
}

func TestHealthContractsBoundedRetention(t *testing.T) {
	t.Parallel()
	key := healthContractKey{organization: "org_example", project: "prj_example", item: "wi_example"}
	cache := healthContractCache{entries: make(map[healthContractKey]healthContractResult)}
	for i := range healthReadLimit {
		old := healthContractKey{item: strconv.Itoa(i)}
		cache.order = append(cache.order, old)
		cache.entries[old] = healthContractResult{}
	}
	for _, test := range []struct {
		name     string
		revision tracker.Revision
	}{
		{name: "evicts oldest at capacity", revision: 1},
		{name: "updates existing without evicting", revision: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache.remember(key, healthContractResult{revision: test.revision, policyID: "policy"})
			if _, stale := cache.entries[healthContractKey{item: "0"}]; stale {
				t.Fatal("oldest entry retained")
			}
			if len(cache.entries) != healthReadLimit || len(cache.order) != healthReadLimit || cache.oldest != 1 || cache.entries[key].revision != test.revision {
				t.Fatalf("entries=%d order=%d oldest=%d revision=%d", len(cache.entries), len(cache.order), cache.oldest, cache.entries[key].revision)
			}
		})
	}
}

func TestHealthContractsLiveInputs(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"linked intake", "release delegation"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
			f := newNativeFixture(t, service, "org_first", "health-live")
			issue := f.create(t, "candidate")
			originalContract := issue.IssueContract
			var source eventstore.LinkedSource
			if name == "linked intake" {
				issue.Body = "Incomplete contract"
				issue.Revision++
				seedHealthContractIssue(t, f, issue)
				source = eventstore.LinkedSource{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, URL: "https://github.com/example-org/example-project/issues/1", TitleSupplied: true, BodySupplied: true}
				if err := service.config.TenantRepository.WriteTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					source, err = w.CreateLinkedSource(t.Context(), source)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				issue.IssueContract = &issuecontract.State{ReleaseDelivery: &issuecontract.ReleaseDeliveryApproval{PolicyID: "previous", BodySHA256: "previous"}}
				issue.Revision++
				seedHealthContractIssue(t, f, issue)
			}
			original := service.config.TenantRepository
			queries := &healthContractQueries{Repository: original, SnapshotRepository: original.(tenantstore.SnapshotRepository)}
			t.Cleanup(func() { service.config.TenantRepository = original })
			var cache healthContractCache
			for step := range 3 {
				service.config.TenantRepository = original
				if step == 2 {
					if name == "linked intake" {
						ref, err := eventstore.NewContent(service.runDetailStore()).PrepareLinkedIssueSnapshot(t.Context(), issue.OrganizationID, tracker.GitHubIssueSnapshot{URL: source.URL})
						if err != nil {
							t.Fatal(err)
						}
						source.SnapshotObjectKey, source.SnapshotSHA256, source.SnapshotBytes, source.SnapshotContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
						if err := original.WriteTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
							_, err := w.UpdateLinkedSource(t.Context(), source)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					} else {
						issue.IssueContract = originalContract
						issue.Revision++
						seedHealthContractIssue(t, f, issue)
					}
				}
				project, descriptor, err := service.readHealthContractProject(t.Context(), issue.OrganizationID, issue.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				service.config.TenantRepository = queries
				queries.issues = 0
				result, err := cache.readRecords(t.Context(), service, project, issue.WorkItemID, issue.Revision, descriptor)
				wait := result.wait
				if err != nil {
					t.Fatal(err)
				}
				wantWait := name == "linked intake" && step == 2 || name == "release delegation" && step < 2
				if queries.issues != 1 || (wait != nil) != wantWait {
					t.Fatalf("step=%d loads=%d wait=%+v, want load and wait=%t", step, queries.issues, wait, wantWait)
				}
			}
		})
	}
}
