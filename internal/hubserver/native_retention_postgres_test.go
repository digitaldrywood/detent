package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresNativeRetentionRepairsProjectionsWithoutLosingHistory(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	old := now.Add(-90 * 24 * time.Hour)
	objects := &memoryRunDetails{}
	content := eventstore.NewContent(objects)
	const project tracker.ProjectID = "example"
	const item tracker.NativeWorkItemID = "wi_closed"
	const pending = corestore.MaxPageSize + 1
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	versions := make(map[tracker.OrganizationID]eventstore.CollaborationVersion)
	bodies := make(map[tracker.OrganizationID][]byte)
	encode := func(value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, org := range organizations {
		state, actorKind, actor := "Done", "human", "principal_"+string(org)
		body := encode(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: item, Number: 1, Revision: 1}, Title: string(org), Body: "Retained closed work", State: state, CreatedAt: old, UpdatedAt: old})
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: item, RecordID: string(item), Revision: 1}, body)
		if err != nil {
			t.Fatal(err)
		}
		versions[org], bodies[org] = version, body
		events := make([]eventstore.Event, 0, pending+1)
		for index := range pending + 1 {
			value := tracker.CollaborationEvent{OrganizationID: org, ProjectID: project, AggregateType: "issue", AggregateID: item, ID: fmt.Sprintf("event_%d", index), Type: "run.checkpointed", SchemaVersion: 1, Actor: tracker.Actor{Kind: actorKind, PrincipalID: actor}, RecordedAt: old}
			if index == 0 {
				value.Type = "workflow.transitioned"
				value.Data.FromState, value.Data.ToState = "Todo", state
			}
			row, err := content.PrepareEvent(t.Context(), org, encode(value))
			if err != nil {
				t.Fatal(err)
			}
			row.PushKnown = index == 0
			events = append(events, row)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: old}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: item, Number: 1, Title: string(org), WorkflowStateName: &state, ActorKind: &actorKind, ActorPrincipalID: &actor, CreatedAt: old, UpdatedAt: old}); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
				return err
			}
			for _, event := range events {
				if _, err := w.AppendEvent(t.Context(), event); err != nil {
					return err
				}
			}
			for _, key := range []string{"expired", "live"} {
				created := old
				if key == "live" {
					created = now
				}
				receipt := receiptstore.Record{OrganizationID: org, ActorID: actor, Operation: "POST /example", CommandKey: key, RequestHash: strings.Repeat("a", 64), CreatedAt: created, PayloadObjectKey: version.PayloadObjectKey, PayloadSHA256: version.PayloadSHA256, PayloadBytes: version.PayloadBytes, PayloadContentBytes: version.PayloadContentBytes, ResponseReplayUntilNs: created.Add(receiptstore.ReplayRetention).UnixNano()}
				if _, err := w.PutReceipt(t.Context(), receipt); err != nil {
					return err
				}
				challenge := corestore.DuplicateChallenge{OrganizationID: org, ProjectID: project, Token: "dup_" + strings.Repeat("a", 32), PrincipalID: actor, RequestID: key, ContentHash: strings.Repeat("b", 64), PolicyHash: strings.Repeat("c", 64), CreatedAt: created, ExpiresAt: created.Add(time.Hour), Candidates: []corestore.DuplicateCandidate{{WorkItemID: item, Number: 1, Revision: 1}}}
				if key == "live" {
					challenge.Token = "dup_" + strings.Repeat("d", 32)
				}
				if err := w.(corestore.DuplicateChallengeWriter).CreateDuplicateChallenge(t.Context(), challenge); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		metric := metricsstore.Metric{At: old, Route: "GET /example", CallerKind: "browser", CallerID: actor, StatusClass: 2, Latency: metricsstore.Latency{Count: 1, Sum: 1, Max: 1, Histogram: [64]int64{1}}}
		if err := repository.MergeRequestMetrics(t.Context(), org, old, []metricsstore.Metric{metric}); err != nil {
			t.Fatal(err)
		}
	}
	for index, org := range organizations {
		t.Run(string(org), func(t *testing.T) {
			service := &Service{config: Config{TenantRepository: repository, AccessRepository: repository, Hosted: &HostedConfig{OrganizationID: string(org)}, Logger: discardLogger(), now: func() time.Time { return now }}, database: &database{postgres: repository}, requestMetrics: &tenantRequestMetrics{repository: repository, organization: org}}
			service.config.RunDetails = objects
			for pass := range 2 {
				if err := service.maintainNativeRetention(t.Context(), now); err != nil {
					t.Fatal(err)
				}
				var unresolved int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND NOT push_known`, org).Scan(&unresolved); err != nil {
					t.Fatal(err)
				}
				if want := max(0, pending-(pass+1)*corestore.MaxPageSize); unresolved != want {
					t.Fatalf("projection pass %d left %d records, want %d", pass, unresolved, want)
				}
			}
			if index == 0 {
				var untouched int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_command_states WHERE organization_id=$1 AND NOT response_expired`, organizations[1]).Scan(&untouched); err != nil || untouched != 2 {
					t.Fatalf("foreign tenant receipts changed: count=%d error=%v", untouched, err)
				}
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				stored, err := r.CollaborationVersion(t.Context(), string(item), 1)
				if err != nil {
					return err
				}
				raw, err := content.CollaborationVersion(t.Context(), stored)
				if err != nil || !bytes.Equal(raw, bodies[org]) {
					t.Fatalf("closed issue payload changed: error=%v", err)
				}
				events, err := r.Events(t.Context(), project, item, 0, corestore.MaxPageSize)
				if err != nil {
					return err
				}
				if len(events) != corestore.MaxPageSize || events[0].Type != "workflow.transitioned" {
					t.Fatalf("retained history page=%d", len(events))
				}
				if _, err := content.Event(t.Context(), events[0]); err != nil {
					return err
				}
				tail, err := r.Events(t.Context(), project, item, events[len(events)-1].Sequence, corestore.MaxPageSize)
				if err != nil {
					return err
				}
				if len(tail) != 2 || tail[1].Sequence != pending+1 {
					t.Fatalf("retained history tail=%+v", tail)
				}
				if _, err := content.Event(t.Context(), tail[1]); err != nil {
					return err
				}
				facts, err := r.(reportstore.Reader).ReadOverviewFacts(t.Context(), reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: old.Add(-time.Hour), Split: old.Add(-time.Minute), To: now, Limit: 10})
				if err != nil {
					return err
				}
				if len(facts) != 1 || facts[0].Title != string(org) || facts[0].CompletedAt == nil || !facts[0].CompletedAt.Equal(old) {
					t.Fatalf("closed report facts=%+v", facts)
				}
				object, err := r.(artifactstore.Reader).ArchiveObjectByReference(t.Context(), eventstore.Reference(stored.PayloadObjectKey, stored.PayloadSHA256, stored.PayloadBytes, stored.PayloadContentBytes))
				if err != nil || object.Reference.Key != versions[org].PayloadObjectKey {
					t.Fatalf("still-owned archive disappeared: error=%v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var expired bool
			var requestHash string
			var owners, challenges, minutes, days int
			if err := admin.QueryRowContext(t.Context(), `SELECT response_expired,request_hash FROM native_command_states WHERE organization_id=$1 AND command_key='expired'`, org).Scan(&expired, &requestHash); err != nil {
				t.Fatal(err)
			}
			if requestHash != strings.Repeat("a", 64) {
				t.Fatalf("expired receipt lost its idempotency hash: %q", requestHash)
			}
			if err := admin.QueryRowContext(t.Context(), `SELECT
 (SELECT count(*) FROM archive_object_owners WHERE organization_id=$1),
 (SELECT count(*) FROM native_duplicate_challenges WHERE organization_id=$1),
 (SELECT count(*) FROM request_minutes WHERE organization_id=$1),
 (SELECT sum(count) FROM request_days WHERE organization_id=$1)`, org).Scan(&owners, &challenges, &minutes, &days); err != nil {
				t.Fatal(err)
			}
			if !expired || owners != 1 || challenges != 1 || minutes != 0 || days != 1 {
				t.Fatalf("cleanup expired=%t owners=%d challenges=%d minutes=%d day_requests=%d", expired, owners, challenges, minutes, days)
			}
		})
	}
}
