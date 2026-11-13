package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func claimNativeAttempt(t *testing.T, f nativeFixture, worker, hostname, session string, item tracker.NativeWorkItemID, policyIDs ...string) tracker.NativeLease {
	t.Helper()
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(worker), "", "")
	if err != nil || status != http.StatusOK || credential.Runner.MachineID == "" {
		t.Fatalf("attempt runner authentication status=%d: %v", status, err)
	}
	machineID := credential.Runner.MachineID
	report := map[string]any{"id": machineID, "hostname": hostname, "display_name": "Example runner", "version": "test", "capacity": 1, "backend_isolation": isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, report), http.StatusOK)
	report["capacity_configuration"] = runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: f.service.config.now()}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(machineID)+"/heartbeat", worker, report), http.StatusOK)
	policyID := hubTestPolicy().ID
	if len(policyIDs) > 0 {
		policyID = policyIDs[0]
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{
		PolicyID: policyID, WorkItemID: item, MachineID: machineID, SessionID: session,
		TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
	})
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	return lease
}

func reportRunnerCapacity(t *testing.T, service *Service, projectBase, credential string, machine tracker.MachineID, displayName string, backends isolation.Report, capacity int) {
	t.Helper()
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, projectBase+"/machines/"+string(machine)+"/heartbeat", credential, map[string]any{
		"display_name": displayName, "capacity": capacity, "version": "test", "backend_isolation": backends,
		"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, Manageable: true, ObservedAt: service.config.now()},
	}), http.StatusOK)
}

func nativeStartedEvent(lease tracker.NativeLease) tracker.NativeRunEvent {
	return tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start"}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"},
			LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: lease.PolicyID, RunID: hubapi.NewID("run"), AttemptID: hubapi.NewID("attempt")}}
}

func nativeTestCheckpoint() *tracker.NativeCheckpoint {
	return &tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", ExternalEffect: "none", EffectState: "none"}
}

func nativeAttemptTestDatabase(t *testing.T, f nativeFixture) *sql.DB {
	t.Helper()
	admin, ok := projectRecordDatabases.Load(f.service.config.TenantRepository)
	if !ok {
		t.Fatal("native attempt fixture has no isolated PostgreSQL database")
	}
	return admin.(*sql.DB)
}

func nativeAttemptListingPayload(t *testing.T, f nativeFixture, id string, raw []byte) {
	t.Helper()
	organization := f.project.OrganizationID
	ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(organization), raw)
	if err != nil {
		t.Fatalf("store listing payload: %v", err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: organization, ID: hubapi.NewID("archive"), Reference: ref, CreatedAt: f.service.config.now().UTC()})
		return err
	}); err != nil {
		t.Fatalf("register listing payload: %v", err)
	}
	if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), "UPDATE native_attempts SET payload_object_key=$1 WHERE organization_id=$2 AND id=$3", ref.Key, organization, id); err != nil {
		t.Fatal(err)
	}
}

func nativeAttemptListingScope(t *testing.T, f nativeFixture) hubscope.NativeScope {
	t.Helper()
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("authenticate listing fixture: status=%d error=%v", status, err)
	}
	return hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
}

func nativeAttemptFixtureLease(t *testing.T, runner runnerFixture, issue tracker.NativeIssue, session string, duration ...time.Duration) tracker.NativeLease {
	t.Helper()
	ttl := time.Hour
	if len(duration) > 0 {
		ttl = duration[0]
	}
	f := runner.nativeFixture
	var issueID int64
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := r.WorkItem(t.Context(), issue.ProjectID, issue.WorkItemID)
		issueID = item.ID
		return err
	}); err != nil {
		t.Fatalf("read attempt work item %s: %v", issue.WorkItemID, err)
	}
	cancellation, err := f.service.prepareExpiredNativeLeaseCancellation(t.Context(), hubscope.NativeScope{Organization: issue.OrganizationID, Project: issue.ProjectID}, issueID, true)
	if err != nil {
		t.Fatalf("prepare lost attempt cancellation: %v", err)
	}
	var claimed leasestore.Lease
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		item, err := w.WorkItem(t.Context(), issue.ProjectID, issue.WorkItemID)
		if err != nil {
			return fmt.Errorf("read attempt work item %s: %w", issue.WorkItemID, err)
		}
		claimed, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(runner.binding.MachineID), SessionID: session, LeaseID: hubapi.NewID("lease"), TTL: ttl, PreviousAttemptCancellation: cancellation.attemptCancellation()})
		if err != nil {
			return fmt.Errorf("claim work item: %w", err)
		}
		now := f.service.config.now().UTC()
		if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: issue.OrganizationID, LeaseID: claimed.LeaseID, RunnerID: runner.binding.RunnerID, AssignedAt: now}); err != nil {
			return fmt.Errorf("assign attempt runner: %w", err)
		}
		if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, LeaseID: tracker.LeaseID(claimed.LeaseID), PolicyID: hubTestPolicy().ID, BoundAt: now}); err != nil {
			return fmt.Errorf("bind attempt policy: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("claim attempt lease: %v", err)
	}
	return tracker.NativeLease{ID: tracker.LeaseID(claimed.LeaseID), WorkItemID: issue.WorkItemID, MachineID: runner.binding.MachineID, SessionID: claimed.SessionID, FencingToken: tracker.FencingToken(claimed.FencingToken), PolicyID: hubTestPolicy().ID, ExpiresAt: claimed.ExpiresAt}
}

func nativeAttemptListingFixture(t *testing.T, count, bodyBytes int) (nativeFixture, tracker.NativeIssue, []tracker.NativeRunData) {
	t.Helper()
	var frozen atomic.Pointer[time.Time]
	f := newDefaultNativeFixture(t, Config{RunDetails: &verifiedAttemptDetails{}, now: func() time.Time {
		if now := frozen.Load(); now != nil {
			return *now
		}
		return time.Now().UTC()
	}})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "attempt listing")
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.enroll(t)
	worker := runner.redemption.Credential
	var records []tracker.NativeRunData
	for index := range count {
		lease := nativeAttemptFixtureLease(t, runner, issue, fmt.Sprintf("listing-session-%d", index))
		start := nativeStartedEvent(lease)
		start.IdempotencyKey = fmt.Sprintf("listing-start-%d", index)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, start), http.StatusOK)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
		attempt, err := f.service.readNativeAttemptTenant(t.Context(), nativeAttemptListingScope(t, f), string(issue.WorkItemID), start.Data.AttemptID)
		if err != nil {
			t.Fatalf("read released listing attempt %d: %v", index, err)
		}
		attempt.CompletionBody = strings.Repeat("x", bodyBytes)
		encoded, err := json.Marshal(attempt.NativeRunData)
		if err != nil {
			t.Fatal(err)
		}
		nativeAttemptListingPayload(t, f, attempt.AttemptID, encoded)
		records = append(records, attempt.NativeRunData)
	}
	now := time.Now().UTC()
	frozen.Store(&now)
	return f, issue, records
}

func TestNativeAttemptNotFoundAcrossTenants(t *testing.T) {
	service := newPostgresNativeService(t, Config{RunDetails: &verifiedAttemptDetails{}})
	fixtures := []nativeFixture{
		newNativeFixture(t, service, "org_first", "first"),
		newNativeFixture(t, service, "org_first_second", "second"),
	}
	var issues []tracker.NativeIssue
	var attempts []string
	for index, f := range fixtures {
		service.localOrganization = string(f.project.OrganizationID)
		approveHubTestPolicy(t, service, f.base+"/policy", hubTestPolicy())
		issue := f.create(t, "attempt lookup")
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/attempts", f.token, nil), http.StatusOK)
		runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		runner.enroll(t)
		lease := nativeAttemptFixtureLease(t, runner, issue, fmt.Sprintf("lookup-%d", index))
		start := nativeStartedEvent(lease)
		ref, err := service.runDetailStore().StoreDetail(t.Context(), string(f.project.OrganizationID), []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: start.Data.AttemptID, RunID: start.Data.RunID,
				Fence: leasestore.Fence{LeaseID: string(lease.ID), FencingToken: int64(lease.FencingToken)}, Sequence: 1, Status: "running", PayloadObjectKey: ref.Key, PayloadReference: ref})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		issues = append(issues, issue)
		attempts = append(attempts, start.Data.AttemptID)
	}
	for index, f := range fixtures {
		t.Run(string(f.project.OrganizationID), func(t *testing.T) {
			service.localOrganization = string(f.project.OrganizationID)
			item := f.base + "/work-items/" + string(issues[index].WorkItemID)
			foreign := 1 - index
			for _, test := range []struct{ name, path string }{
				{"missing item", f.base + "/work-items/wi_missing/attempts"},
				{"missing attempt", item + "/attempts/attempt_missing"},
				{"foreign item", f.base + "/work-items/" + string(issues[foreign].WorkItemID) + "/attempts"},
				{"foreign attempt", item + "/attempts/" + attempts[foreign]},
			} {
				t.Run(test.name, func(t *testing.T) {
					requireNativeCode(t, performHubAPIRequest(t, service, http.MethodGet, test.path, f.token, nil), http.StatusNotFound, "not_found")
				})
			}
			for _, test := range []struct{ name, suffix string }{
				{"missing event page", "/attempts"},
				{"missing event detail", "/attempts/" + attempts[index]},
			} {
				t.Run(test.name, func(t *testing.T) {
					requireNativeCode(t, performHubAPIRequest(t, service, http.MethodGet, item+test.suffix, f.token, nil), http.StatusNotFound, "not_found")
				})
			}
		})
	}
}

func TestNativeAttemptsSkipUnusedHydration(t *testing.T) {
	for _, test := range []struct {
		name      string
		limit     int
		bodyBytes int
		corrupt   int
		want      int
	}{
		{"count lookahead", 1, 0, 1, 1},
		{"after byte boundary", 100, 400 << 10, 3, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, issue, records := nativeAttemptListingFixture(t, 4, test.bodyBytes)
			nativeAttemptListingPayload(t, f, records[test.corrupt].AttemptID, []byte(`{"attempt_id":42}`))
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/attempts?view=detail&limit="+strconv.Itoa(test.limit), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page tracker.Page[tracker.NativeAttempt]
			decodeHubResponse(t, response, &page)
			if len(page.Items) != test.want || page.NextCursor == "" {
				t.Fatalf("items=%d cursor=%q, want %d and continuation", len(page.Items), page.NextCursor, test.want)
			}
		})
	}
}

func legacyNativeAttemptPage(attempts []tracker.NativeAttempt, limit, budget int, cursor nativeCursor, key []byte, view string) (tracker.Page[tracker.NativeAttempt], error) {
	page := tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{}}
	hasMore := false
	for _, attempt := range attempts {
		if len(page.Items) == limit {
			hasMore = true
			break
		}
		next := cursor
		next.After = strconv.FormatInt(int64(attempt.FencingToken), 10)
		candidate := tracker.Page[tracker.NativeAttempt]{Items: append(page.Items, attempt)}
		var err error
		candidate.NextCursor, err = encodeNativeCursor(next, key)
		if err != nil {
			return page, err
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return page, err
		}
		if len(encoded)+1 > budget && len(page.Items) == 0 && view != "" && view != "detail" {
			candidate.Items[0] = nativeAttemptAuthorityProjection(attempt)
			encoded, err = json.Marshal(candidate)
			if err != nil {
				return page, err
			}
		}
		if len(encoded)+1 > budget {
			if len(page.Items) == 0 {
				return tracker.Page[tracker.NativeAttempt]{}, &hubapi.NativeError{Code: "attempt_record_too_large", Status: http.StatusUnprocessableEntity}
			}
			hasMore = true
			break
		}
		page.Items = candidate.Items
		cursor = next
	}
	if hasMore {
		var err error
		page.NextCursor, err = encodeNativeCursor(cursor, key)
		if err != nil {
			return page, err
		}
	}
	return page, nil
}

func TestNativeAttemptPageByteBoundaryEquivalence(t *testing.T) {
	f, issue, records := nativeAttemptListingFixture(t, 5, 0)
	scope := nativeAttemptListingScope(t, f)
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/attempts"
	base, err := f.service.readAttempts(t.Context(), scope, string(issue.WorkItemID), url.Values{"view": {"detail"}, "limit": {"100"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"detail", "blockers", "recorded_blockers"} {
		for _, direction := range []string{"oldest", "newest"} {
			params := url.Values{"view": {view}, "direction": {direction}}
			_, cursor, key, err := f.service.readNativePage(t.Context(), scope, path, params)
			if err != nil {
				t.Fatal(err)
			}
			budget := 1 << 20
			if view != "" && view != "detail" {
				budget = operatortool.WorkHistoryPageBytes
			}
			ordered := append([]tracker.NativeAttempt(nil), base.Items...)
			if view == "recorded_blockers" {
				for index := range ordered {
					ordered[index].Unavailable = []string{"evidence"}
				}
			}
			if direction == "newest" {
				for left, right := 0, len(ordered)-1; left < right; left, right = left+1, right-1 {
					ordered[left], ordered[right] = ordered[right], ordered[left]
				}
			}
			next := cursor
			next.After = strconv.FormatInt(int64(ordered[0].FencingToken), 10)
			nextCursor, err := encodeNativeCursor(next, key)
			if err != nil {
				t.Fatal(err)
			}
			first := ordered[0]
			first.CompletionBody = "x"
			one, err := json.Marshal(tracker.Page[tracker.NativeAttempt]{Items: []tracker.NativeAttempt{first}, NextCursor: nextCursor})
			if err != nil {
				t.Fatal(err)
			}
			exact := budget - len(one)
			for _, size := range []int{0, 1000, exact - 1, exact, exact + 1, budget + 1} {
				body := strings.Repeat("x", size)
				for _, record := range records {
					record.CompletionBody = body
					encoded, err := json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
					nativeAttemptListingPayload(t, f, record.AttemptID, encoded)
				}
				for index := range ordered {
					ordered[index].CompletionBody = body
				}
				for _, limit := range []int{1, 2, 5, 100} {
					t.Run(fmt.Sprintf("%s/%s/bytes=%d/limit=%d", view, direction, size, limit), func(t *testing.T) {
						params := url.Values{"view": {view}, "direction": {direction}}
						params.Set("limit", strconv.Itoa(limit))
						remaining := ordered
						current := cursor
						for {
							want, wantErr := legacyNativeAttemptPage(remaining, limit, budget, current, key, view)
							got, err := f.service.readNativeAttemptPage(t.Context(), scope, string(issue.WorkItemID), params, json.Marshal)
							if wantErr != nil {
								var oversized *hubapi.NativeError
								if !errors.As(err, &oversized) || oversized.Code != "attempt_record_too_large" || oversized.Status != http.StatusUnprocessableEntity {
									t.Fatalf("error=%v, want oversized record", err)
								}
								break
							}
							if err != nil || !reflect.DeepEqual(got.Page, want) {
								t.Fatalf("page mismatch: items=%d want=%d error=%v", len(got.Items), len(want.Items), err)
							}
							encoded, err := json.Marshal(want)
							if err != nil || !bytes.Equal(got.encoded, append(encoded, '\n')) || len(got.encoded) > budget {
								t.Fatalf("response encoding differs: bytes=%d error=%v", len(got.encoded), err)
							}
							if want.NextCursor == "" {
								break
							}
							if len(want.Items) == 0 {
								t.Fatal("cursor did not advance")
							}
							remaining = remaining[len(want.Items):]
							current.After = strconv.FormatInt(int64(want.Items[len(want.Items)-1].FencingToken), 10)
							params.Set("cursor", want.NextCursor)
						}
					})
				}
			}
		}
	}
}

func TestNativeAttemptPageEncodesEachReturnedRecordOnce(t *testing.T) {
	for _, test := range []struct {
		name       string
		bodyBytes  int
		limit      int
		want       int
		considered int
	}{
		{"count limit", 0, 2, 2, 2},
		{"byte limit", 400 << 10, 100, 2, 3},
		{"last page", 1000, 100, 5, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, issue, _ := nativeAttemptListingFixture(t, 5, test.bodyBytes)
			counts := map[string]int{}
			marshal := func(value any) ([]byte, error) {
				attempt, ok := value.(tracker.NativeAttempt)
				if !ok {
					t.Fatalf("encoded growing page prefix: %T", value)
				}
				counts[attempt.AttemptID]++
				return json.Marshal(attempt)
			}
			got, err := f.service.readNativeAttemptPage(t.Context(), nativeAttemptListingScope(t, f), string(issue.WorkItemID), url.Values{"view": {"detail"}, "limit": {strconv.Itoa(test.limit)}}, marshal)
			if err != nil || len(got.Items) != test.want || len(counts) != test.considered {
				t.Fatalf("items=%d encodes=%v error=%v", len(got.Items), counts, err)
			}
			for id, count := range counts {
				if count != 1 {
					t.Fatalf("attempt %s encoded %d times", id, count)
				}
			}
		})
	}
}

func BenchmarkNativeAttemptPageEncoding(b *testing.B) {
	for _, count := range []int{20, 100} {
		attempts := make([]tracker.NativeAttempt, count)
		for index := range attempts {
			attempts[index] = tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: fmt.Sprintf("attempt_%d", index), FencingToken: tracker.FencingToken(index + 1), CompletionBody: strings.Repeat("x", 6000)}}
		}
		cursor := nativeCursor{Version: 2, Scope: "example", Expires: 1800000000}
		key := []byte("example-cursor-key")
		b.Run(fmt.Sprintf("prefix/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				page, err := legacyNativeAttemptPage(attempts, count, 1<<20, cursor, key, "")
				if err != nil {
					b.Fatal(err)
				}
				if _, err := json.Marshal(page); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("incremental/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				page := nativeAttemptPage{encoded: []byte(`{"items":[`)}
				for _, attempt := range attempts {
					next := cursor
					next.After = strconv.FormatInt(int64(attempt.FencingToken), 10)
					nextCursor, err := encodeNativeCursor(next, key)
					if err != nil {
						b.Fatal(err)
					}
					encoded, err := json.Marshal(attempt)
					if err != nil || page.candidateBytes(encoded, nextCursor) > 1<<20 {
						b.Fatalf("unexpected page overflow or error: %v", err)
					}
					page.appendAttempt(attempt, encoded)
				}
				page.finish()
			}
		})
	}
}

func reopenNativeAttemptService(t *testing.T, previous *Service) *Service {
	t.Helper()
	admin := nativeAttemptTestDatabase(t, nativeFixture{service: previous})
	config := previous.config
	config.TenantRepository, config.AccessRepository, config.BillingRepository = nil, nil, nil
	config.NativeRunEvents, config.ConversationDispatch, config.ProjectParticipants = nil, nil, nil
	store, err := openPostgresDatabase(t.Context(), *config.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	repository := store.postgres
	projectRecordDatabases.Store(repository, admin)
	t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
	config.Hosted = startupConfig(config.Postgres.Connection).Hosted
	restarted := openLocalPostgresHubTestService(t, repository, config)
	restarted.database.hostedOrganization, restarted.localOrganization, restarted.requestMetrics.Organization = previous.database.hostedOrganization, previous.localOrganization, previous.requestMetrics.Organization
	if restarted.config.Postgres.Connection.Database != previous.config.Postgres.Connection.Database || restarted.config.TenantRepository == previous.config.TenantRepository {
		t.Fatal("attempt restart changed database or retained closed repository")
	}
	return restarted
}

func appendNativeAttemptHistoryFixture(t *testing.T, f nativeFixture, value tracker.CollaborationEvent) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.project.OrganizationID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), prepared); return err }); err != nil {
		t.Fatal(err)
	}
}

func copyNativeAttemptFixture(t *testing.T, f nativeFixture, worker string, issue tracker.NativeIssue, first tracker.NativeLease, sourceID string, index int) tracker.NativeLease {
	t.Helper()
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(worker), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("copy runner status=%d: %v", status, err)
	}
	source, err := f.service.readNativeAttemptTenant(t.Context(), nativeAttemptListingScope(t, f), string(issue.WorkItemID), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if source.FencingToken != first.FencingToken {
		t.Fatal("copy source changed")
	}
	var template leasestore.Attempt
	var checkpoint attachment.ArchiveReference
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		template, err = r.Attempt(t.Context(), source.AttemptID)
		if err != nil {
			return err
		}
		reader, ok := r.(attemptArchiveReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		object, err := reader.ArchiveObjectByKey(t.Context(), hubapi.StringValue(template.CheckpointObjectKey))
		checkpoint = object.Reference
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runner := runnerFixture{nativeFixture: f, binding: credential.Runner.Binding}
	lease := nativeAttemptFixtureLease(t, runner, issue, fmt.Sprintf("historical-%d", index), 90*time.Second)
	start := nativeStartedEvent(lease)
	for _, sequence := range []int64{1, 2, 3} {
		data := start.Data
		status := "running"
		if sequence == 3 {
			data = source.NativeRunData
			data.AttemptID, data.RunID, data.LeaseID, data.FencingToken, data.SessionID = start.Data.AttemptID, start.Data.RunID, lease.ID, lease.FencingToken, lease.SessionID
			status = "succeeded"
		} else if sequence == 2 {
			data.Handoff = nativeTestCheckpoint()
		}
		data.Sequence = sequence
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(f.project.OrganizationID), raw)
		if err != nil {
			t.Fatal(err)
		}
		event := leasestore.AttemptEvent{ID: data.AttemptID, Fence: leasestore.Fence{LeaseID: string(lease.ID), FencingToken: int64(lease.FencingToken)}, RunID: data.RunID, Sequence: sequence, Status: status, StartedAt: source.StartedAt, PayloadReference: ref, PayloadObjectKey: ref.Key, Usage: data.Usage}
		if sequence > 1 {
			event.CheckpointObjectKey, event.CheckpointPreservesSource, event.CheckpointReference = template.CheckpointObjectKey, template.CheckpointPreservesSource, checkpoint
		}
		if data.Runtime != nil {
			event.RuntimeHeartbeatAt = new(data.Runtime.HeartbeatAt)
		}
		if data.Disposition != nil && data.Disposition.HumanAction {
			event.HumanActionStatus = new(data.Disposition.Status)
		}
		var history *eventstore.Event
		if sequence == 3 {
			actor := (hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}).Actor()
			value := tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, Actor: actor, RecordedAt: f.service.config.now(), Data: tracker.CollaborationData{Run: &data}}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.project.OrganizationID, raw)
			if err != nil {
				t.Fatal(err)
			}
			history = &prepared
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			attempt, err := w.RecordAttempt(t.Context(), event)
			if err != nil {
				return err
			}
			if err := w.SetAttemptClaimFacts(t.Context(), attemptClaimFacts(attempt, nativeAttemptClaimDisposition(data, source.Checkpoint, issue.ProjectID), false)); err != nil {
				return err
			}
			if history != nil {
				_, err = w.AppendEvent(t.Context(), *history)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	return lease
}

func TestNativeOrderedAttemptLifecycle(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "work")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	forged := start
	forged.IdempotencyKey = "forged-evidence"
	forged.Data.Evidence = []tracker.NativeEvidence{{AttachmentID: "att_forged", Caption: "Not uploaded", Reference: "![forged](attachment:att_forged)"}}
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	checkpoint := start
	checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "checkpoint", 2
	checkpoint.Data.Handoff = nativeTestCheckpoint()
	finish := start
	finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 3, "succeeded"
	reconnect := finish
	reconnect.Type, reconnect.IdempotencyKey, reconnect.Data.Sequence, reconnect.Data.Outcome = "run.observed", "reconnect-terminal", 4, ""
	reconnect.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now()}
	invalid := reconnect
	invalid.IdempotencyKey, invalid.Data.Sequence = "invalid-observation", 2
	invalid.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", LocalAttemptID: 4, Generation: 9, Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 99}, Identity: agentidentity.Identity{BackendID: "sentinel-private-value"}}
	invalidCheckpoint := invalid
	invalidCheckpoint.Type, invalidCheckpoint.IdempotencyKey, invalidCheckpoint.Data.Handoff = "run.checkpointed", "invalid-checkpoint", nativeTestCheckpoint()
	invalidValidation := invalid
	invalidValidation.IdempotencyKey = "invalid-validation"
	invalidValidation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Validation: &gate.CommandResult{Command: "sentinel-private-value", Output: "sentinel-private-output"}}
	foreignPreProvider := start
	foreignPreProvider.Type, foreignPreProvider.IdempotencyKey, foreignPreProvider.Data.Outcome = "run.finished", "foreign-pre-provider-session", "cancelled"
	foreignPreProvider.Data.Identity = nil
	foreignPreProvider.Data.SessionID = "another-session"
	foreignPreProvider.Data.TerminalFailure = &tracker.NativeTerminalFailure{ObservedAt: time.Now().UTC(), Stage: "pre_provider", ErrorClass: "lease_released", Error: "dispatch deferred"}

	lifecycleTest := t
	startedPublished := false
	for _, test := range []struct {
		name     string
		event    tracker.NativeRunEvent
		status   int
		restart  bool
		revision int64
	}{
		{"evidence without upload", forged, http.StatusUnprocessableEntity, false, 0},
		{"invalid runtime observation", invalid, http.StatusUnprocessableEntity, false, 0},
		{"invalid checkpoint observation", invalidCheckpoint, http.StatusUnprocessableEntity, false, 0},
		{"invalid source validation", invalidValidation, http.StatusUnprocessableEntity, false, 0},
		{"foreign pre-provider session", foreignPreProvider, http.StatusUnprocessableEntity, false, 0},
		{"checkpoint before start", checkpoint, http.StatusConflict, false, 0},
		{"start", start, http.StatusOK, false, 0},
		{"same command", start, http.StatusOK, false, 0},
		{"completion skips checkpoint", finish, http.StatusConflict, false, 0},
		{"checkpoint", checkpoint, http.StatusOK, false, 1},
		{"complete after restart", finish, http.StatusOK, true, 2},
		{"duplicate completion", finish, http.StatusOK, false, 2},
		{"completed attempt cannot reconnect", reconnect, http.StatusConflict, false, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.restart {
				started := time.Now()
				if err := f.service.Shutdown(testenv.WaitContext(t, context.Background())); err != nil {
					t.Fatal(err)
				}
				if err := f.service.Close(); err != nil {
					t.Fatal(err)
				}
				f.service = reopenNativeAttemptService(lifecycleTest, f.service)
				t.Logf("hub_restart_seconds=%.6f", time.Since(started).Seconds())
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90})
				requireNativeStatus(t, response, http.StatusOK)
				var renewed tracker.NativeLease
				decodeHubResponse(t, response, &renewed)
				if renewed.ID != lease.ID || renewed.FencingToken != lease.FencingToken || renewed.SessionID != lease.SessionID {
					t.Fatalf("restart changed execution ownership: %+v", renewed)
				}
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{PolicyID: lease.PolicyID, WorkItemID: issue.WorkItemID, MachineID: lease.MachineID, SessionID: "replacement", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
				requireNativeStatus(t, response, http.StatusConflict)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, test.event)
			requireNativeStatus(t, response, test.status)
			if test.name == "foreign pre-provider session" {
				var result struct {
					Details struct {
						Rejection struct{ Category, Field string }
					}
				}
				decodeHubResponse(t, response, &result)
				if result.Details.Rejection.Category != "owner_validation" || result.Details.Rejection.Field != "data" {
					t.Fatalf("foreign session bypassed lease ownership: %+v", result)
				}
			}
			if test.name == "start" {
				startedPublished = true
			}
			var projectedSequence int64
			if err := nativeAttemptTestDatabase(t, f).QueryRowContext(t.Context(), "SELECT COALESCE((SELECT e.run_sequence FROM report_attempt_facts a JOIN report_event_facts e ON e.organization_id=a.organization_id AND e.event_id=a.latest_event_id WHERE a.organization_id=$1 AND a.attempt_id=$2),0)", f.project.OrganizationID, start.Data.AttemptID).Scan(&projectedSequence); err != nil {
				t.Fatal(err)
			}
			wantSequence := test.revision
			if startedPublished {
				wantSequence++
			}
			if projectedSequence != wantSequence {
				t.Fatalf("report projected sequence=%d, want %d", projectedSequence, wantSequence)
			}
			if strings.Contains(response.Body.String(), "sentinel-private") {
				t.Fatal("public rejection disclosed private payload")
			}
			if test.name == "invalid runtime observation" || test.name == "invalid checkpoint observation" || test.name == "invalid source validation" {
				var result struct {
					Details struct {
						Rejection struct{ Category, Field string }
					}
				}
				decodeHubResponse(t, response, &result)
				field := "data.runtime.activity"
				if test.name == "invalid source validation" {
					field = "data.runtime.validation"
				}
				if result.Details.Rejection.Category != "runtime_validation" || result.Details.Rejection.Field != field {
					t.Fatalf("lost structural rejection: %+v", result)
				}
			}
		})
	}
	for _, event := range []tracker.NativeRunEvent{start, checkpoint, finish} {
		event.IdempotencyKey += "-new-key"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
	}
	for _, test := range []struct {
		name string
		edit func(*tracker.NativeRunEvent)
	}{
		{"different terminal result", func(e *tracker.NativeRunEvent) { e.Data.Outcome = "failed" }},
		{"late progress", func(e *tracker.NativeRunEvent) {
			e.Type = "run.checkpointed"
			e.Data.Sequence = 4
			e.Data.Outcome = ""
			e.Data.Handoff = nativeTestCheckpoint()
		}},
		{"second attempt on same lease", func(e *tracker.NativeRunEvent) { *e = nativeStartedEvent(lease) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := finish
			test.edit(&event)
			event.IdempotencyKey = test.name
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusConflict)
		})
	}
	var full tracker.Page[tracker.NativeAttempt]
	for _, view := range []string{"", "blockers", "recorded_blockers"} {
		response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+url.Values{"view": {view}}.Encode(), worker, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var attempts tracker.Page[tracker.NativeAttempt]
		decodeHubResponse(t, response, &attempts)
		if len(attempts.Items) != 1 || attempts.Items[0].Sequence != 3 || attempts.Items[0].Status != "succeeded" || attempts.Items[0].Checkpoint == nil || attempts.Items[0].Checkpoint.WorktreeState != "dirty" || attempts.Items[0].MachineID != lease.MachineID || attempts.Items[0].SessionID != lease.SessionID {
			t.Fatalf("view=%q attempt projection = %#v", view, attempts)
		}
		if attempts.Items[0].Runtime != nil || attempts.Items[0].RuntimeFreshness != "unavailable" {
			t.Fatalf("view=%q fabricated runtime evidence: %+v", view, attempts.Items[0])
		}
		if view == "" {
			full = attempts
		} else if !reflect.DeepEqual(attempts, full) {
			t.Fatalf("view=%q changed attempt without runtime", view)
		}
	}
	var count int
	if err := nativeAttemptTestDatabase(t, f).QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND type LIKE 'run.%'", f.project.OrganizationID, f.project.ID, issue.WorkItemID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("run event count = %d, error = %v", count, err)
	}
}

func TestNativeRecoveryAfterReassignmentAndRestart(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		retained bool
		closed   bool
	}{
		{name: "expired attempt"},
		{name: "released retained source after runner loss", retained: true},
		{name: "closed unresolved source survives retention and restart", retained: true, closed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			clock := &leaseTestClock{value: now}
			config := Config{Clock: clock.Now, RunDetails: &memoryRunDetails{}}
			f := newNativeFixture(t, newClockedPostgresNativeService(t, config), "", "recovery")
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "work")
			owner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
			owner.enroll(t)
			owner.reportCapacity(t)
			worker, firstMachine := owner.redemption.Credential, string(owner.binding.MachineID)
			claim := func(token, machine, session string) tracker.NativeLease {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", token, tracker.NativeClaim{
					PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: tracker.MachineID(machine), SessionID: session,
					TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
				})
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				return lease
			}
			lease := claim(worker, firstMachine, "first-session")
			event := nativeStartedEvent(lease)
			path := f.base + "/work-items/" + string(issue.WorkItemID)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "push-ambiguity", 2
			event.Data.Handoff = nativeTestCheckpoint()
			event.Data.Handoff.ExternalEffect, event.Data.Handoff.EffectState, event.Data.Handoff.EffectID = "git_push", "ambiguous", hubapi.NewID("effect")
			event.Data.Handoff.HeadSHA = strings.Repeat("a", 40)
			event.Data.Handoff.ExpectedHeadSHA = strings.Repeat("b", 40)
			var version tracker.ChangeVersion
			var bundle []byte
			var originalCheckout, successorCheckout, changePath string
			git := func(directory string, args ...string) string {
				t.Helper()
				command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory, "-c", "user.name=Recovery Fixture", "-c", "user.email=recovery@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
				for _, value := range os.Environ() {
					if !strings.HasPrefix(value, "GIT_") {
						command.Env = append(command.Env, value)
					}
				}
				command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("recovery fixture git %v: %s: %v", args, output, err)
				}
				return strings.TrimSpace(string(output))
			}
			if test.retained {
				input := changeTestInput()
				input.AttemptID, input.RunID = event.Data.AttemptID, event.Data.RunID
				input.Repository = "https://github.com/example-org/example-project"
				bundle, originalCheckout = changeTestSourceCheckout(t, &input, []byte("retained recovery source\n"))
				event.Data.Handoff.HeadSHA = input.HeadSHA
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/changes", f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Retained recovery source", Body: "Source custody"})
				requireNativeStatus(t, response, http.StatusOK)
				var change tracker.ChangeRequest
				decodeHubResponse(t, response, &change)
				changePath = path + "/changes/" + change.ID
				response = performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "retained-source"}, ChangeVersionInput: input, SourceBundle: bundle})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &version)
				origin := t.TempDir()
				git(origin, "init", "--bare", "--object-format=sha1", "--template=")
				git(origin, "fetch", originalCheckout, input.BaseSHA+":refs/heads/main")
				git(origin, "symbolic-ref", "HEAD", "refs/heads/main")
				successorCheckout = t.TempDir()
				git(successorCheckout, "init", "-b", "main", "--object-format=sha1", "--template=")
				git(successorCheckout, "fetch", origin, "main")
				git(successorCheckout, "reset", "--hard", "FETCH_HEAD")
				git(successorCheckout, "remote", "add", "origin", input.Repository)
				git(successorCheckout, "config", "url."+origin+".insteadOf", input.Repository)
			}
			wantCheckpoint := *event.Data.Handoff
			event.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 168, Generation: 27, Phase: "implementation", HeartbeatAt: now, Phases: []tracker.NativePhase{{Name: "implementation", StartedAt: now}}, Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 168, Generation: 27, Dropped: 3, Unpaired: 2, Coverage: "partial", AsOf: now}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), http.StatusOK)
			for _, elapsed := range []time.Duration{30 * time.Second, 70 * time.Second} {
				clock.Advance(elapsed)
				now = clock.Now()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
			}
			if test.retained {
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					attempt, err := r.Attempt(t.Context(), event.Data.AttemptID)
					if err != nil {
						return err
					}
					if attempt.PayloadObjectKey == "" || attempt.CheckpointObjectKey == nil || *attempt.CheckpointObjectKey == "" {
						t.Fatal("attempt or checkpoint is not reference-backed")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			var evidence tracker.NativeRuntimeEvidence
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/runtime", worker, nil), &evidence)
			if !evidence.Attempt.Current || evidence.Attempt.RuntimeFreshness != "expired" || !reflect.DeepEqual(evidence.Attempt.Checkpoint, &wantCheckpoint) {
				t.Fatalf("renewal manufactured activity freshness=%#v", evidence.Attempt)
			}
			if test.retained {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
				if err := os.RemoveAll(originalCheckout); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = reopenNativeAttemptService(t, f.service)
			clock.Advance(90*time.Second + time.Nanosecond)
			now = clock.Now()
			if test.retained {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), owner.binding.RunnerID)
					if err != nil {
						return err
					}
					identity.LastHeartbeatAt = now.Add(-runnerauth.HeartbeatTimeout - time.Minute)
					return w.UpdateRunnerIdentity(t.Context(), identity)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.closed {
				move := func(state string) {
					t.Helper()
					current := readWorkItem(t, f, issue.WorkItemID, "")
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "retention-" + state}, ExpectedRevision: current.Revision, State: state, Reason: "user_requested"}), http.StatusOK)
				}
				move("Done")
				if err := f.service.maintainNativeRetention(t.Context(), now.Add(73*time.Hour)); err != nil {
					t.Fatal(err)
				}
				if err := f.service.Close(); err != nil {
					t.Fatal(err)
				}
				f.service = reopenNativeAttemptService(t, f.service)
				if current := readWorkItem(t, f, issue.WorkItemID, ""); current.Archived || current.State != "Done" {
					t.Fatal("retention or restart lost the live closed unresolved issue")
				}
				var retained tracker.NativeRecovery
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/recovery", worker, nil), &retained)
				if retained.ChangeDetail == nil || len(retained.ChangeDetail.Versions) != 1 || retained.ChangeDetail.Versions[0].ID != version.ID || !reflect.DeepEqual(retained.ChangeDetail.Versions[0].Source, version.Source) {
					t.Fatal("retention lost unresolved immutable source")
				}
				move("Todo")
			}
			refuseEvidence := func(mutation tracker.Mutation) {
				t.Helper()
				credential, _, err := f.service.authenticateAPIToken(t.Context(), worker, "", "")
				if err != nil {
					t.Fatal(err)
				}
				scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
				err = f.service.checkAttachmentEvidenceTenant(t.Context(), scope, attachment.EvidenceRequest{Mutation: mutation, WorkItemID: issue.WorkItemID, AttemptID: event.Data.AttemptID, Caption: "Expired evidence"})
				want := "stale_execution"
				if test.retained && mutation.LeaseID != "" && mutation.LeaseID != lease.ID {
					want = "not_found"
				}
				var failure *hubapi.NativeError
				if !errors.As(err, &failure) || failure.Code != want {
					t.Fatalf("stale evidence was not fenced: %v", err)
				}
			}
			refuseEvidence(tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken})
			other, successorMachine := worker, firstMachine
			if test.retained {
				successor := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
				successor.enroll(t)
				successor.reportCapacity(t)
				other, successorMachine = successor.redemption.Credential, string(successor.binding.MachineID)
				if successorMachine == firstMachine || successor.binding.RunnerID == owner.binding.RunnerID {
					t.Fatal("recovery reused the original runner")
				}
			}
			replacement := claim(other, successorMachine, "other-session")
			if replacement.FencingToken <= lease.FencingToken {
				t.Fatal("lease reassignment did not advance fencing")
			}
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/runtime?native_attempt_id="+event.Data.AttemptID, other, nil), &evidence)
			if evidence.Attempt.Current || evidence.Attempt.Status != "cancelled" || evidence.Attempt.RuntimeFreshness != "available" || evidence.Attempt.Runtime.Activity.Dropped != 3 || evidence.Attempt.Runtime.Activity.Unpaired != 2 || evidence.CurrentLease == nil || evidence.CurrentLease.ID != replacement.ID || evidence.Scheduling.Outcome != "claimed" {
				t.Fatalf("restart lost runtime observation=%#v", evidence.Attempt)
			}
			if test.retained && (evidence.Attempt.ClaimReleasedAt == nil || !reflect.DeepEqual(evidence.Attempt.Checkpoint, &wantCheckpoint)) {
				t.Fatalf("released attempt lost its exact checkpoint: %+v", evidence.Attempt)
			}
			for _, test := range []struct {
				name     string
				mutation tracker.Mutation
			}{
				{"omitted fence", tracker.Mutation{IdempotencyKey: "omitted-fence"}},
				{"expired owner", tracker.Mutation{IdempotencyKey: "expired-owner", LeaseID: lease.ID, FencingToken: lease.FencingToken}},
			} {
				t.Run(test.name, func(t *testing.T) {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", worker, tracker.CreateComment{Mutation: test.mutation, Body: "stale mutation"}), http.StatusConflict)
					refuseEvidence(test.mutation)
				})
			}
			refuseEvidence(tracker.Mutation{LeaseID: replacement.ID, FencingToken: replacement.FencingToken})
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", other, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "new-owner", LeaseID: replacement.ID, FencingToken: replacement.FencingToken}, Body: "current owner"}), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "operator"}, Body: "operator collaboration"}), http.StatusOK)
			for _, operation := range []string{"renew", "release", "events"} {
				t.Run("stale "+operation, func(t *testing.T) {
					url := f.base + "/leases/" + string(lease.ID) + "/" + operation
					var request any = tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90, Reason: "completed"}
					if operation == "events" {
						url = path + "/events"
						event.IdempotencyKey, event.Type, event.Data.Sequence, event.Data.Outcome, event.Data.Handoff = "late-finish", "run.finished", 3, "succeeded", nil
						request = event
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, url, worker, request), http.StatusConflict)
				})
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?limit=1", other, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page tracker.Page[tracker.NativeAttempt]
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || page.Items[0].Status != "cancelled" || page.Items[0].Checkpoint == nil || page.Items[0].Checkpoint.EffectState != "ambiguous" || page.Items[0].Checkpoint.WorktreeState != "dirty" {
				t.Fatalf("recovery lost evidence: %#v", page)
			}
			next := nativeStartedEvent(replacement)
			next.IdempotencyKey = "replacement-start"
			next.Data.RunID = event.Data.RunID
			next.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 168, Generation: 28, Phase: "rework", HeartbeatAt: now}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", other, next), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/runtime?attempt_id=168", other, nil), http.StatusUnprocessableEntity)
			response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?limit=1", other, nil)
			decodeHubResponse(t, response, &page)
			if page.NextCursor == "" {
				t.Fatal("attempt history was not paginated")
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?limit=1&cursor="+page.NextCursor, other, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || page.Items[0].FencingToken != replacement.FencingToken || page.Items[0].Status != "running" {
				t.Fatalf("successor page = %#v", page)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?direction=newest&limit=1", other, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || page.Items[0].FencingToken != replacement.FencingToken || page.NextCursor == "" {
				t.Fatalf("newest attempt page = %#v", page)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?direction=newest&limit=1&cursor="+url.QueryEscape(page.NextCursor), other, nil)
			requireNativeStatus(t, response, http.StatusOK)
			page = tracker.Page[tracker.NativeAttempt]{}
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || page.Items[0].FencingToken != lease.FencingToken || page.NextCursor != "" {
				t.Fatalf("older attempt page = %#v", page)
			}
			var recovery tracker.NativeRecovery
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"?view=recovery", other, nil), &recovery)
			if len(recovery.Attempts) != 2 || recovery.Attempts[0].Checkpoint == nil || recovery.Attempts[0].Checkpoint.EffectState != "ambiguous" || string(recovery.Attempts[0].MachineID) != firstMachine || recovery.Attempts[1].AttemptID != next.Data.AttemptID || recovery.Attempts[1].Checkpoint != nil {
				t.Fatalf("fresh startup lost the previous owned checkpoint: %+v", recovery.Attempts)
			}
			if test.retained {
				sourceAttempt := recovery.SourceAttempt()
				if recovery.SourceAttemptID != event.Data.AttemptID || sourceAttempt == nil || !reflect.DeepEqual(sourceAttempt.Checkpoint, &wantCheckpoint) || recovery.ChangeDetail == nil || len(recovery.ChangeDetail.Versions) != 1 {
					t.Fatalf("successor lost retained source custody: %+v", recovery)
				}
				recoveredVersion := recovery.ChangeDetail.Versions[0]
				if recoveredVersion.ID != version.ID || !reflect.DeepEqual(recoveredVersion.Source, version.Source) {
					t.Fatal("recovery selected a different immutable source")
				}
				response := performHubAPIRequest(t, f.service, http.MethodGet, changePath+"/versions/"+recoveredVersion.ID+"/source", other, nil)
				requireNativeStatus(t, response, http.StatusOK)
				if !bytes.Equal(response.Body.Bytes(), bundle) {
					t.Fatal("successor retrieved different retained source bytes")
				}
				backend, err := workspace.NewLocalGitContext(t.Context(), workspace.LocalGitOptions{SourceRoot: successorCheckout, Root: filepath.Join(t.TempDir(), "workspaces"), AutoBranch: true, CommitIdentity: workspace.CommitIdentity{Name: "Recovery Fixture", Email: "recovery@example.invalid"}})
				if err != nil {
					t.Fatal(err)
				}
				restored, err := backend.Create(t.Context(), workspace.Issue{Identifier: "native#1", NativeRework: true, BaseRef: recoveredVersion.BaseSHA, Source: &workspace.ChangeSource{Version: recoveredVersion, Bundle: response.Body.Bytes()}})
				if err != nil {
					t.Fatalf("restore retained source on successor: %v", err)
				}
				if git(restored.Path, "rev-parse", "HEAD") != wantCheckpoint.HeadSHA {
					t.Fatal("restored source differs from checkpoint head")
				}
				content, err := os.ReadFile(filepath.Join(restored.Path, "source.txt"))
				if err != nil || string(content) != "retained recovery source\n" {
					t.Fatalf("restored source bytes=%q error=%v", content, err)
				}
			}
			if len(recovery.Discussion) != 2 || recovery.Discussion[1].Body != "operator collaboration" || len(recovery.History) != 0 {
				t.Fatalf("compact recovery lost current human context: %+v", recovery)
			}
		})
	}
}

func TestNativeConcurrentOrderedEvents(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{RunDetails: &memoryRunDetails{}})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "work")
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.enroll(t)
	worker := runner.redemption.Credential
	event := nativeStartedEvent(nativeAttemptFixtureLease(t, runner, issue, "concurrent-session"))
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := range 8 {
		group.Go(func() {
			<-start
			request := event
			request.IdempotencyKey = fmt.Sprintf("concurrent-%d", index)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, request), http.StatusOK)
		})
	}
	close(start)
	group.Wait()
	var count int
	if err := nativeAttemptTestDatabase(t, f).QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2", issue.OrganizationID, event.Data.AttemptID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate progress: %d, %v", count, err)
	}
}

func TestNativePreProviderCancellationValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*tracker.NativeRunData)
		valid  bool
	}{
		{name: "before runtime observation", valid: true},
		{name: "lease session identity", change: func(data *tracker.NativeRunData) { data.SessionID = "session_example" }, valid: true},
		{name: "after runtime observation", change: func(data *tracker.NativeRunData) {
			data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 1, Generation: 1}
		}, valid: true},
		{name: "native runtime without local store", change: func(data *tracker.NativeRunData) {
			data.Runtime = &tracker.NativeRuntimeObservation{Generation: 1}
		}, valid: true},
		{name: "native setup failure without local store", change: func(data *tracker.NativeRunData) {
			data.Runtime = &tracker.NativeRuntimeObservation{Generation: 1}
			data.Outcome = "failed"
			data.TerminalFailure.ErrorClass = "workspace_hook"
		}, valid: true},
		{name: "negative local runtime identity", change: func(data *tracker.NativeRunData) {
			data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: -1, Generation: 1}
		}},
		{name: "partial runtime identity", change: func(data *tracker.NativeRunData) {
			data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 1}
		}},
		{name: "missing failure", change: func(data *tracker.NativeRunData) { data.TerminalFailure = nil }},
		{name: "unobserved unrelated failure", change: func(data *tracker.NativeRunData) { data.TerminalFailure.ErrorClass = "workspace_hook" }},
		{name: "provider success", change: func(data *tracker.NativeRunData) { data.Outcome = "succeeded" }},
		{name: "unobserved setup failure", change: func(data *tracker.NativeRunData) { data.Outcome = "failed" }},
		{name: "later sequence", change: func(data *tracker.NativeRunData) { data.Sequence = 2 }},
		{name: "unobserved source", change: func(data *tracker.NativeRunData) { data.Handoff = nativeTestCheckpoint() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := tracker.NativeRunData{Sequence: 1, Outcome: "cancelled", TerminalFailure: &tracker.NativeTerminalFailure{
				ObservedAt: time.Now().UTC(), Stage: "pre_provider", ErrorClass: "lease_released", Error: "attempt cancelled before provider start; release reason: dispatch_deferred",
			}}
			if test.change != nil {
				test.change(&data)
			}
			if err := validateNativeExecution(data, "run.finished"); (err == nil) != test.valid {
				t.Fatalf("cancellation validation = %v, want valid %t", err, test.valid)
			}
		})
	}
}

func TestNativeDispositionValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		summary string
		valid   bool
	}{
		{"legacy empty summary", "", true},
		{"exact bound", strings.Repeat("x", workpad.MaxFinalSummaryBytes), true},
		{"oversized summary", strings.Repeat("x", workpad.MaxFinalSummaryBytes+1), false},
		{"invalid UTF-8 summary", string([]byte{0xff}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "test"}, Disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "instance_limitation", FinalSummary: test.summary}}
			if err := validateNativeExecution(data, "run.finished"); (err == nil) != test.valid {
				t.Fatalf("disposition validation = %v, want valid %t", err, test.valid)
			}
		})
	}
}

func TestNativeCheckpointValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		edit func(*tracker.NativeRunData)
	}{
		{"negative sequence", func(d *tracker.NativeRunData) { d.Sequence = -1 }},
		{"raw identity", func(d *tracker.NativeRunData) { d.Identity.Model = "raw prompt text" }},
		{"missing handoff", func(d *tracker.NativeRunData) { d.Handoff = nil }},
		{"unknown availability", func(d *tracker.NativeRunData) { d.Handoff.Availability = "probably" }},
		{"raw head", func(d *tracker.NativeRunData) { d.Handoff.HeadSHA = "/local/path" }},
		{"unbound effect", func(d *tracker.NativeRunData) { d.Handoff.ExternalEffect = "git_push" }},
		{"customer availability assertion", func(d *tracker.NativeRunData) {
			d.Handoff.Storage = "customer_store"
			d.ArtifactIDs = []string{hubapi.NewID("artifact")}
		}},
		{"untyped change", func(d *tracker.NativeRunData) {
			d.Handoff.Change = &tracker.NativeChangeReference{ChangeID: "https://example.com"}
		}},
		{"git checkpoint without commit", func(d *tracker.NativeRunData) {
			d.Handoff.Storage, d.Handoff.Ref, d.Handoff.TreeSHA = "git_ref", tracker.CheckpointRefPrefix+"wi_1", strings.Repeat("e", 40)
		}},
		{"git checkpoint outside the Detent namespace", func(d *tracker.NativeRunData) {
			d.Handoff.Storage, d.Handoff.Ref, d.Handoff.CommitSHA, d.Handoff.TreeSHA = "git_ref", "refs/heads/main", strings.Repeat("c", 40), strings.Repeat("e", 40)
		}},
		{"git checkpoint ref traversal", func(d *tracker.NativeRunData) {
			d.Handoff.Storage, d.Handoff.Ref, d.Handoff.CommitSHA, d.Handoff.TreeSHA = "git_ref", tracker.CheckpointRefPrefix+"..", strings.Repeat("c", 40), strings.Repeat("e", 40)
		}},
		{"local checkpoint claiming a git ref", func(d *tracker.NativeRunData) {
			d.Handoff.Ref, d.Handoff.CommitSHA, d.Handoff.TreeSHA = tracker.CheckpointRefPrefix+"wi_1", strings.Repeat("c", 40), strings.Repeat("e", 40)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := tracker.NativeRunData{Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}, Handoff: nativeTestCheckpoint()}
			test.edit(&data)
			if err := validateNativeExecution(data, "run.checkpointed"); err == nil {
				t.Fatal("invalid checkpoint accepted")
			}
		})
	}
}

func TestNativeAttemptPageBounds(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "large-attempts")
	worker := f.worker(t, "worker")
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	output := strings.Repeat("<", 64<<10)
	var fences []tracker.FencingToken
	var first tracker.NativeLease
	var firstAttempt string
	for index := range 51 {
		if index > 0 {
			lease := copyNativeAttemptFixture(t, f, worker, issue, first, firstAttempt, index)
			fences = append(fences, lease.FencingToken)
			continue
		}
		lease := claimNativeAttempt(t, f, worker, "machine", fmt.Sprintf("session-%d", index), issue.WorkItemID)
		first = lease
		start := nativeStartedEvent(lease)
		start.IdempotencyKey = fmt.Sprintf("start-%d", index)
		firstAttempt = start.Data.AttemptID
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
		checkpoint := start
		checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", fmt.Sprintf("checkpoint-%d", index), 2
		checkpoint.Data.Handoff = nativeTestCheckpoint()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
		now = now.Add(time.Millisecond)
		finish := start
		finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", fmt.Sprintf("finish-%d", index), 3, "succeeded"
		finish.Data.CompletionBody = "Completed attempt details"
		finish.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "test-model", Input: 123, Output: 45, Currency: "USD"}}
		finish.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", HumanAction: true, FinalSummary: strings.Repeat("x", workpad.MaxFinalSummaryBytes)}
		finish.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: f.service.config.now(),
			Validation: &gate.CommandResult{Command: "focused test", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), DurationNS: 1, Output: output},
			Landing:    &tracker.NativeLandingReceipt{Waiting: true, HeadSHA: strings.Repeat("a", 40)}}
		if err := validateNativeRuntime(finish.Data.Runtime); err != nil {
			t.Fatalf("fixture is not a legitimate runtime: %v", err)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
		var dispositionStatus string
		var dispositionHumanAction bool
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			attempt, err := r.Attempt(t.Context(), finish.Data.AttemptID)
			if err != nil {
				return err
			}
			dispositionHumanAction = hubapi.StringValue(attempt.ClaimDisposition) == "human_action"
			deltas, err := r.BoardDeltas(t.Context(), f.project.ID, 0, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, delta := range deltas {
				if delta.Topic == "human_action" && delta.EntityID == finish.Data.AttemptID && delta.Kind == "upsert" {
					dispositionStatus = delta.Status
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if dispositionStatus != "blocked" || !dispositionHumanAction {
			t.Fatalf("typed disposition=%s/%v", dispositionStatus, dispositionHumanAction)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
		fences = append(fences, lease.FencingToken)
	}
	var full []tracker.NativeAttempt
	for _, view := range []string{"detail", "blockers", "recorded_blockers"} {
		t.Run("view="+view, func(t *testing.T) {
			params := url.Values{"limit": {"100"}}
			if view != "" {
				params.Set("view", view)
			}
			var seen []tracker.FencingToken
			pages := 0
			for {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+params.Encode(), worker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				budget := 1 << 20
				if view != "" && view != "detail" {
					budget = operatortool.WorkHistoryPageBytes
				}
				if response.Body.Len() > budget {
					t.Fatalf("page bytes=%d, budget=%d", response.Body.Len(), budget)
				}
				var page tracker.Page[tracker.NativeAttempt]
				decodeHubResponse(t, response, &page)
				pages++
				for _, attempt := range page.Items {
					seen = append(seen, attempt.FencingToken)
					if attempt.Status != "succeeded" || attempt.Identity == nil || attempt.WorkItemRevision != issue.Revision || attempt.StartedAt.IsZero() || attempt.Disposition == nil || !attempt.Disposition.HumanAction || len(attempt.Disposition.FinalSummary) != workpad.MaxFinalSummaryBytes || !reflect.DeepEqual(attempt.Checkpoint, nativeTestCheckpoint()) {
						t.Fatalf("lost authority/checkpoint at fence %d", attempt.FencingToken)
					}
					if view != "" && view != "detail" {
						if attempt.Runtime != nil || attempt.RuntimeFreshness != "unavailable" {
							t.Fatal("blocker projection included runtime")
						}
					} else {
						if attempt.Runtime == nil || attempt.Runtime.Validation.Output != output || attempt.Runtime.Landing == nil || !attempt.Runtime.Landing.Waiting || attempt.Runtime.Landing.HeadSHA != strings.Repeat("a", 40) {
							t.Fatal("full attempt lost validation or landing evidence")
						}
						full = append(full, attempt)
					}
				}
				if len(seen) > len(fences) {
					t.Fatal("repeated attempts")
				}
				if page.NextCursor == "" {
					break
				}
				if pages == 1 {
					replay := url.Values{"limit": {"1"}, "cursor": {page.NextCursor}}
					if view != "" {
						replay.Set("view", view)
					}
					r := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+replay.Encode(), worker, nil)
					requireNativeStatus(t, r, http.StatusOK)
					var next tracker.Page[tracker.NativeAttempt]
					decodeHubResponse(t, r, &next)
					if len(next.Items) != 1 || next.Items[0].FencingToken != fences[len(seen)] {
						t.Fatal("byte boundary skipped a fence")
					}
					if view == "blockers" {
						replay.Del("view")
					} else {
						replay.Set("view", "blockers")
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+replay.Encode(), worker, nil), http.StatusUnprocessableEntity)
				}
				params.Set("cursor", page.NextCursor)
			}
			if pages < 2 || !reflect.DeepEqual(seen, fences) {
				t.Fatalf("pages=%d, fences=%v, want=%v", pages, seen, fences)
			}
		})
	}
	if full == nil {
		t.Fatal("full attempt view returned no records")
	}
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	for _, transport := range []string{"stdio", "http"} {
		for _, limit := range []int{1, 20, 200} {
			t.Run(fmt.Sprintf("work_runs/%s/%d", transport, limit), func(t *testing.T) {
				call := hostedContextProtocol(t, f.service, ctx, transport)
				args := map[string]any{"project_id": string(f.project.ID), "reference": string(issue.WorkItemID), "limit": limit}
				seen := 0
				for {
					raw := hostedContextData(t, call("tools/call", operatortool.WorkRuns, args), false)
					var result operatortool.WorkReadResult[tracker.Page[tracker.NativeAttempt]]
					if err := json.Unmarshal(raw, &result); err != nil {
						t.Fatal(err)
					}
					if len(raw) > operatortool.MaxResultBytes || len(result.Data.Items) == 0 || len(result.Data.Items) > limit {
						t.Fatalf("invalid run page: bytes=%d items=%d", len(raw), len(result.Data.Items))
					}
					for _, attempt := range result.Data.Items {
						if seen >= len(full) {
							t.Fatal("repeated attempts")
						}
						want := full[seen]
						want.Runtime = nil
						want.RuntimeFreshness = "unavailable"
						if !reflect.DeepEqual(attempt, want) {
							t.Fatalf("summary changed fields at attempt %d", seen)
						}
						seen++
					}
					if result.Data.NextCursor == "" {
						break
					}
					args["cursor"] = result.Data.NextCursor
				}
				if seen != len(full) {
					t.Fatalf("read %d attempts, want %d", seen, len(full))
				}
			})
		}
	}
	if len(full) == 0 {
		t.Fatal("missing full attempt fixtures")
	}
	raw, err := json.Marshal(tracker.Page[tracker.NativeAttempt]{Items: full})
	if err != nil || len(raw) <= 1<<20 {
		t.Fatalf("old count-only payload bytes=%d, error=%v", len(raw), err)
	}
	t.Logf("legitimate count-only attempt payload=%d bytes", len(raw))
	for _, query := range []string{"view=unknown", "view=blockers&view=blockers"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+query, worker, nil), http.StatusUnprocessableEntity)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var detail tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, response, &detail)
	omitted := false
	for _, event := range detail.Items {
		if event.Type == "run.finished" && event.DataOmission != nil {
			omitted = true
		}
	}
	if !omitted {
		t.Fatal("fixture did not exercise large history data omission")
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?view=blockers&limit=100", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var projected tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, response, &projected)
	found := false
	for _, event := range projected.Items {
		if event.Type == "run.finished" && event.Data.Run != nil && event.Data.Run.FencingToken == first.FencingToken {
			found = event.DataOmission == nil && event.Data.Run.AttemptID != "" && event.Data.Run.Runtime == nil
		}
	}
	if !found {
		t.Fatal("blocker history omitted recorded run authority")
	}
	firstRun := full[0].NativeRunData
	edit := tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "issue.edited", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, RecordedAt: f.service.config.now(), Data: tracker.CollaborationData{Revision: 9, Run: &firstRun}}
	appendNativeAttemptHistoryFixture(t, f, edit)
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?view=blockers&limit=100", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &projected)
	found = false
	for _, event := range projected.Items {
		if event.Type == "issue.edited" {
			found = event.Data.Revision == 9 && event.Actor.Kind == "human" && event.DataOmission == nil
		}
	}
	if !found {
		t.Fatal("large human edit lost invalidating revision")
	}
	for _, query := range []string{"view=unknown", "view=blockers&view=blockers"} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?"+query, worker, nil), http.StatusUnprocessableEntity)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?view=blockers&limit=1", worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &projected)
	if projected.NextCursor == "" {
		t.Fatal("missing history continuation")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?cursor="+url.QueryEscape(projected.NextCursor), worker, nil), http.StatusUnprocessableEntity)
	oversized := full[0].NativeRunData
	oversized.Runtime.Validation.Output = strings.Repeat("<", 256<<10)
	encoded, err := json.Marshal(oversized)
	if err != nil {
		t.Fatal(err)
	}
	nativeAttemptListingPayload(t, f, full[0].AttemptID, encoded)
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=detail&limit=1", worker, nil)
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
	if len(full) == 0 {
		t.Fatal("missing full attempt for oversized record identity check")
	}
	if !strings.Contains(response.Body.String(), string(issue.WorkItemID)) || !strings.Contains(response.Body.String(), full[0].AttemptID) {
		t.Fatal("oversized record refusal lost item or attempt identity")
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=blockers&limit=1", worker, nil), http.StatusOK)
	oversized.CompletionBody = strings.Repeat("<", 64<<10)
	encoded, err = json.Marshal(oversized)
	if err != nil {
		t.Fatal(err)
	}
	nativeAttemptListingPayload(t, f, full[0].AttemptID, encoded)
	for _, view := range []string{"blockers", "recorded_blockers"} {
		t.Run("oversized_pagination/"+view, func(t *testing.T) {
			params := url.Values{"view": {view}, "limit": {"200"}}
			seen := 0
			for {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?"+params.Encode(), worker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				if response.Body.Len() > operatortool.WorkHistoryPageBytes {
					t.Fatalf("page exceeded budget: %d", response.Body.Len())
				}
				var page tracker.Page[tracker.NativeAttempt]
				decodeHubResponse(t, response, &page)
				if len(page.Items) == 0 || seen == 0 && len(page.Items) < 2 {
					t.Fatal("oversized attempt prevented neighboring attempts from being returned")
				}
				for _, got := range page.Items {
					if seen >= len(full) {
						t.Fatal("repeated attempt")
					}
					want := full[seen]
					want.Runtime, want.RuntimeFreshness = nil, "unavailable"
					if seen == 0 {
						want.CompletionBody, want.Usage = "", nil
						want.Unavailable = []string{"completion_body", "usage"}
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("attempt %d lost authority, checkpoint or unaffected detail", seen)
					}
					seen++
				}
				if page.NextCursor == "" {
					break
				}
				params.Set("cursor", page.NextCursor)
			}
			if seen != len(full) {
				t.Fatalf("returned %d attempts, want %d", seen, len(full))
			}
		})
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts/"+full[0].AttemptID, worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var retained tracker.NativeAttempt
	decodeHubResponse(t, response, &retained)
	if retained.CompletionBody != strings.Repeat("<", 64<<10) || retained.Runtime == nil || retained.Runtime.Validation.Output != strings.Repeat("<", 256<<10) || len(retained.Unavailable) != 0 {
		t.Fatal("projection changed retained attempt detail")
	}
	for _, transport := range []string{"stdio", "http"} {
		for _, limit := range []int{1, 20, 200} {
			t.Run(fmt.Sprintf("oversized_summary/%s/%d", transport, limit), func(t *testing.T) {
				call := hostedContextProtocol(t, f.service, ctx, transport)
				args := map[string]any{"project_id": string(f.project.ID), "reference": string(issue.WorkItemID), "limit": limit}
				raw := hostedContextData(t, call("tools/call", operatortool.WorkRuns, args), false)
				var result operatortool.WorkReadResult[tracker.Page[tracker.NativeAttempt]]
				if err := json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				if len(raw) > operatortool.MaxResultBytes || len(result.Data.Items) == 0 {
					t.Fatalf("unbounded or empty summary: bytes=%d", len(raw))
				}
				got := result.Data.Items[0]
				want := full[0]
				want.Runtime, want.RuntimeFreshness = nil, "unavailable"
				want.CompletionBody, want.Usage = "", nil
				if !reflect.DeepEqual(got.Unavailable, []string{"completion_body", "usage"}) {
					t.Fatal("omitted attempt fields were not marked unavailable")
				}
				got.Unavailable = nil
				if !reflect.DeepEqual(got, want) {
					t.Fatal("oversized summary changed authority or checkpoint fields")
				}
			})
		}
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other")
	denied := other.worker(t, "denied")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts?view=blockers", denied, nil), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?view=blockers", denied, nil), http.StatusNotFound)
	var historyRecord eventstore.Event
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		rows, err := r.HistoryEvents(t.Context(), f.project.ID, issue.WorkItemID, 0, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Type == "run.finished" {
				historyRecord = row
				return nil
			}
		}
		return corestore.ErrNotFound
	}); err != nil {
		t.Fatal(err)
	}
	historySource, err := eventstore.NewContent(f.service.runDetailStore()).Event(t.Context(), historyRecord)
	if err != nil {
		t.Fatal(err)
	}
	for range 101 {
		historySource.ID, historySource.AggregateSequence = hubapi.NewID("evt"), 0
		appendNativeAttemptHistoryFixture(t, f, historySource)
	}
	started := time.Now()
	response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?view=timeline&direction=newest&limit=100", worker, nil)
	t.Logf("newest timeline history page: elapsed=%s bytes=%d", time.Since(started), response.Body.Len())
	requireNativeStatus(t, response, http.StatusOK)
	var timeline tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, response, &timeline)
	if len(timeline.Items) != 100 || timeline.NextCursor == "" || response.Body.Len() > 100<<10 {
		t.Fatalf("timeline page rows=%d bytes=%d cursor=%t", len(timeline.Items), response.Body.Len(), timeline.NextCursor != "")
	}
	for _, event := range timeline.Items {
		if event.Data.Run != nil || event.DataOmission != nil {
			t.Fatal("timeline loaded large run detail instead of the event envelope")
		}
	}

}

func TestNativeLeaseLossTerminalizesAttempts(t *testing.T) {
	for _, mode := range []string{"superseded", "superseded imported", "released", "expired repair", "released repair", "superseded repair", "revoked", "release rollback"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			clock := &leaseTestClock{value: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
			f := newDefaultNativeFixture(t, Config{now: clock.Now})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			enroll := func() runnerFixture {
				t.Helper()
				runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
				runner.enroll(t)
				reportRunnerCapacity(t, f.service, f.base, runner.redemption.Credential, runner.binding.MachineID, "Example runner", runner.redemption.BackendIsolation, 2)
				return runner
			}
			owner := enroll()
			items := []tracker.NativeIssue{f.create(t, "Lost execution"), f.create(t, "Current execution")}
			claim := func(runner runnerFixture, item tracker.NativeWorkItemID, session string, ttl int64) tracker.NativeLease {
				t.Helper()
				request := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: item, MachineID: runner.binding.MachineID, SessionID: session, TTLSeconds: ttl, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, request)
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				return lease
			}
			leases := []tracker.NativeLease{
				claim(owner, items[0].WorkItemID, "lost-session", 90),
				claim(owner, items[1].WorkItemID, "current-session", 180),
			}
			lease := leases[0]
			organization, project := f.project.OrganizationID, f.project.ID
			repository := f.service.config.TenantRepository
			admin := nativeAttemptTestDatabase(t, f)
			var newerAttempt leasestore.Attempt
			if mode == "superseded imported" {
				if _, err := admin.ExecContext(ctx, `UPDATE leases SET released_at=$1,release_reason='released' WHERE organization_id=$2 AND lease_id=$3`,
					clock.Now(), organization, string(lease.ID)); err != nil {
					t.Fatal(err)
				}
				newerRunner := enroll()
				newer := claim(newerRunner, items[0].WorkItemID, "newer-session", 90)
				event := nativeStartedEvent(newer)
				event.IdempotencyKey = "start_newer"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(items[0].WorkItemID)+"/events", newerRunner.redemption.Credential, event), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(newer.ID)+"/release", newerRunner.redemption.Credential,
					tracker.NativeLeaseMutation{FencingToken: newer.FencingToken, Reason: "completed"}), http.StatusNoContent)
				if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					newerAttempt, err = r.Attempt(ctx, event.Data.AttemptID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if newer.FencingToken <= lease.FencingToken || newerAttempt.Status != "cancelled" {
					t.Fatalf("expected newer terminal attempt: %+v", newerAttempt)
				}
				if _, err := admin.ExecContext(ctx, `UPDATE leases SET released_at=NULL,release_reason='' WHERE organization_id=$1 AND lease_id=$2`,
					organization, string(lease.ID)); err != nil {
					t.Fatal(err)
				}
			}
			events := make([]tracker.NativeRunEvent, len(items))
			for i, item := range items {
				events[i] = nativeStartedEvent(leases[i])
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/events", owner.redemption.Credential, events[i]), http.StatusOK)
			}
			if err := f.service.maintainBoardRecords(ctx); err != nil {
				t.Fatal(err)
			}

			switch mode {
			case "released", "release rollback":
				want := http.StatusNoContent
				if mode == "release rollback" {
					if _, err := admin.ExecContext(ctx, `CREATE FUNCTION reject_attempt_cancel() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'cancel failed'; END $$`); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.ExecContext(ctx, `CREATE TRIGGER reject_attempt_cancel BEFORE UPDATE OF status ON native_attempts FOR EACH ROW WHEN (NEW.status = 'cancelled') EXECUTE FUNCTION reject_attempt_cancel()`); err != nil {
						t.Fatal(err)
					}
					want = http.StatusInternalServerError
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", owner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), want)
			case "revoked":
				if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(ctx, owner.binding.RunnerID)
					if err != nil {
						return err
					}
					return w.RevokeAccessCredential(ctx, identity.TokenID, clock.Now())
				}); err != nil {
					t.Fatal(err)
				}
			default:
				clock.Advance(lease.ExpiresAt.Sub(clock.Now()))
				if mode == "released repair" || mode == "superseded repair" {
					result, err := admin.ExecContext(ctx, `UPDATE leases SET released_at=$1, release_reason='released' WHERE organization_id=$2 AND lease_id=$3 AND fencing_token=$4`, clock.Now(), organization, string(lease.ID), int64(lease.FencingToken))
					if err != nil {
						t.Fatal(err)
					}
					if n, err := result.RowsAffected(); err != nil || n != 1 {
						t.Fatalf("seed one released lease: rows=%d error=%v", n, err)
					}
				}
				if mode == "superseded" || mode == "superseded imported" || mode == "superseded repair" {
					replacement := enroll()
					current := claim(replacement, items[0].WorkItemID, "replacement-session", 90)
					if current.FencingToken <= lease.FencingToken || int64(current.FencingToken) <= newerAttempt.FencingToken {
						t.Fatal("replacement reused fencing token")
					}
				}
				if mode != "superseded" && mode != "superseded imported" {
					if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
						now := clock.Now()
						usage, err := r.ClaimCapacityUsage(ctx, string(owner.binding.MachineID), owner.binding.RunnerID, project, "", now)
						if err != nil {
							return err
						}
						if usage.Host != 1 || usage.Runner != 1 || usage.Project != 1 {
							t.Errorf("stale attempt consumed capacity: %+v", usage)
						}
						fleet, err := r.(runnerstore.FleetReader).FleetLeases(ctx, now, 10)
						if err != nil {
							return err
						}
						var assigned []tracker.NativeWorkItemID
						for _, row := range fleet {
							if row.RunnerID == owner.binding.RunnerID {
								assigned = append(assigned, row.Lease.WorkItemID)
							}
						}
						if len(assigned) != 1 || assigned[0] != items[1].WorkItemID {
							t.Errorf("runner fleet leases=%v, want only live peer %s", assigned, items[1].WorkItemID)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if err := f.service.maintainBoardRecords(ctx); err != nil {
						t.Fatal(err)
					}
					scope := nativeAttemptListingScope(t, f)
					board, err := f.service.readBoardRecordsFor(ctx, scope, url.Values{"include": {"board"}}, true)
					if err != nil {
						t.Fatal(err)
					}
					if mode != "superseded repair" {
						found := false
						for _, card := range board.Cards {
							if card.Issue.WorkItemID == items[0].WorkItemID {
								found = true
								if card.Execution != nil {
									t.Errorf("expired execution remained on board: %+v", card.Execution)
								}
							}
						}
						if !found {
							t.Error("expired work item missing from board")
						}
					}
					if board.Work == nil {
						t.Fatal("board summary missing")
					}
					running := 0
					for _, lane := range board.Work.Lanes {
						running += lane.Running
					}
					if running != 1 {
						t.Errorf("board counted stale attempts: running=%d", running)
					}
				}
				if mode == "expired repair" {
					scope := nativeAttemptListingScope(t, f)
					var issueID int64
					if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
						item, err := r.WorkItem(ctx, project, items[0].WorkItemID)
						issueID = item.ID
						return err
					}); err != nil {
						t.Fatal(err)
					}
					prepared, err := f.service.prepareExpiredNativeLeaseCancellation(ctx, scope, issueID, false)
					if err != nil {
						t.Fatal(err)
					}
					if prepared == nil {
						t.Fatal("expired running attempt has no cancellation")
					}
					if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.ExpireWithCancellation(ctx, leasestore.Fence{LeaseID: string(lease.ID), FencingToken: int64(lease.FencingToken)}, prepared.attemptCancellation())
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}

			if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
				if newerAttempt.ID != "" {
					current, err := r.Attempt(ctx, newerAttempt.ID)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(current, newerAttempt) {
						t.Errorf("replacement changed newer terminal attempt: before=%+v after=%+v", newerAttempt, current)
					}
				}
				if mode == "release rollback" {
					stored, err := r.Lease(ctx, string(lease.ID))
					if err != nil {
						return err
					}
					if stored.ReleasedAt != nil {
						t.Error("lease release committed without attempt cancellation")
					}
				}
				for i, event := range events {
					attempt, err := r.Attempt(ctx, event.Data.AttemptID)
					if err != nil {
						return err
					}
					want := "running"
					if mode != "release rollback" && (i == 0 || mode == "revoked") {
						want = "cancelled"
					}
					if attempt.Status != want {
						t.Errorf("item %d stored status=%s, want %s", i, attempt.Status, want)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			scope := nativeAttemptListingScope(t, f)
			for i, event := range events {
				if mode == "release rollback" || i != 0 && mode != "revoked" {
					continue
				}
				attempt, err := f.service.readNativeAttemptTenant(ctx, scope, string(items[i].WorkItemID), event.Data.AttemptID)
				if err != nil {
					t.Errorf("read item %d retained attempt: %v", i, err)
					continue
				}
				if attempt.Outcome != "cancelled" || attempt.TerminalFailure == nil || attempt.TerminalFailure.ErrorClass != "lease_released" {
					t.Errorf("item %d missing typed lease cancellation: sequence=%d outcome=%q failure=%+v", i, attempt.Sequence, attempt.Outcome, attempt.TerminalFailure)
				}
			}
		})
	}
}

func TestExpiredLeaseCancellationWithoutRunEvent(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "expired attempt without run evidence")
	organization, project := f.project.OrganizationID, f.project.ID
	claimed := claimNativeAttempt(t, f, f.worker(t, "worker"), "example-host", "session_example", issue.WorkItemID)
	var lease leasestore.Lease
	var issueID int64
	attemptID, runID := hubapi.NewID("attempt"), hubapi.NewID("run")
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		lease, err = w.Lease(t.Context(), string(claimed.ID))
		if err != nil {
			return err
		}
		issueID = lease.IssueID
		_, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: attemptID, RunID: runID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, Sequence: 1, Status: "running", StartedAt: f.service.config.now().UTC(), PayloadObjectKey: "example/attempt"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), `UPDATE leases SET acquired_at=acquired_at-interval '3 minutes',renewed_at=renewed_at-interval '2 minutes',expires_at=expires_at-interval '2 minutes' WHERE organization_id=$1 AND lease_id=$2`, organization, lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	prepared, err := f.service.prepareExpiredNativeLeaseCancellation(t.Context(), hubscope.NativeScope{Organization: organization, Project: project}, issueID, false)
	if err != nil || prepared == nil {
		t.Fatalf("expired attempt without run event: prepared=%v error=%v", prepared, err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.ExpireWithCancellation(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, prepared.attemptCancellation())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		expired, err := r.Lease(t.Context(), lease.LeaseID)
		if err != nil {
			return err
		}
		attempt, err := r.Attempt(t.Context(), attemptID)
		if err != nil {
			return err
		}
		if expired.ReleasedAt == nil || expired.ReleaseReason != "expired" || attempt.Status != "cancelled" || attempt.Sequence != 2 {
			return fmt.Errorf("expiry lease=%+v attempt=%+v", expired, attempt)
		}
		event, err := r.LatestRunEvent(t.Context(), project, attemptID)
		if err != nil {
			return err
		}
		stored, err := eventstore.NewContent(f.service.runDetailStore()).Event(t.Context(), event)
		if err != nil {
			return err
		}
		run := stored.Data.Run
		if event.Type != "run.finished" || stored.AggregateID != issue.WorkItemID || run == nil || run.AttemptID != attemptID || run.RunID != runID || run.Outcome != "cancelled" || run.Sequence != 2 || run.PolicyID != hubTestPolicy().ID || string(run.LeaseID) != lease.LeaseID || int64(run.FencingToken) != lease.FencingToken || run.TerminalFailure == nil {
			return fmt.Errorf("cancelled run event=%+v run=%+v", event, run)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
