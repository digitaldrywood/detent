package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRetention(t *testing.T) {
	t.Run("claim reviews retain each reviewer across historical backfill", func(t *testing.T) {
		f := newChangeFixture(t, newClockedPostgresNativeService(t, WithDeps(Config{}, hubdeps.Deps{RetentionTimeout: frozenRetentionTimeout})))
		first := f.publish(t, "first", "", true)
		second := f.publish(t, "second", first.ID, true)
		content := eventstore.NewContent(f.service.runDetailStore())
		var reviews []tracker.ChangeReview
		for index := range hubcontent.ClaimEvidenceLimit + 1 {
			decision := "commented"
			if index == 0 {
				decision = "changes_requested"
			}
			reviews = append(reviews, tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: first.ID, Actor: tracker.Actor{Kind: "human", PrincipalID: "reviewer-one"}, Decision: decision, CreatedAt: f.service.config.Clock()})
		}
		for _, version := range []string{first.ID, second.ID} {
			reviews = append(reviews,
				tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: version, Actor: tracker.Actor{Kind: "human", PrincipalID: "reviewer-one"}, Decision: "approved", CreatedAt: f.service.config.Clock()},
				tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: version, Actor: tracker.Actor{Kind: "human", PrincipalID: "reviewer-two"}, Decision: "changes_requested", CreatedAt: f.service.config.Clock()})
		}
		appendRetentionReviewEvidence(t, f, reviews)
		assertReviews := func(version string, expected map[string]string) {
			t.Helper()
			selected := readRetentionReviewEvidence(t, f, version)
			decisions := make(map[string]string)
			for _, review := range selected {
				decisions[review.Actor.PrincipalID] = review.Decision
			}
			if len(selected) != len(expected) || !reflect.DeepEqual(decisions, expected) {
				t.Fatalf("review authority = %+v, want %+v", selected, expected)
			}
		}
		admin := usagePostgresAdmin(t, f.service)
		var traceMu sync.Mutex
		statements, schemaChanges := 0, 0
		backfillContext := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
			traceMu.Lock()
			defer traceMu.Unlock()
			statements++
			query = strings.ToUpper(strings.TrimSpace(query))
			for strings.HasPrefix(query, "--") {
				_, query, _ = strings.Cut(query, "\n")
				query = strings.TrimSpace(query)
			}
			for _, prefix := range []string{"CREATE ", "ALTER ", "DROP ", "TRUNCATE "} {
				if strings.HasPrefix(query, prefix) {
					schemaChanges++
				}
			}
		})
		repository := f.service.config.TenantRepository.(*postgres.Repository)
		converted := 0
		for range (len(reviews)+6)/7 + 1 {
			count, err := repository.BackfillChangeRecoveryProjections(backfillContext, f.project.OrganizationID, content, 7)
			if err != nil || count < 0 || count > 7 {
				t.Fatalf("bounded review backfill = %d: %v", count, err)
			}
			converted += count
		}
		if converted != len(reviews) {
			t.Fatalf("projected review history = %d, want %d", converted, len(reviews))
		}
		for _, version := range []string{first.ID, second.ID} {
			assertReviews(version, map[string]string{"reviewer-one": "approved", "reviewer-two": "changes_requested"})
		}
		for _, update := range []struct{ actor, decision string }{{"reviewer-two", "approved"}, {"reviewer-one", "changes_requested"}} {
			appendRetentionReviewEvidence(t, f, []tracker.ChangeReview{{ID: hubapi.NewID("review"), VersionID: first.ID, Actor: tracker.Actor{Kind: "human", PrincipalID: update.actor}, Decision: update.decision, CreatedAt: f.service.config.Clock()}})
			selected := readRetentionReviewEvidence(t, f, first.ID)
			if !slices.ContainsFunc(selected, func(review tracker.ChangeReview) bool {
				return review.Actor.PrincipalID == update.actor && review.Decision == update.decision
			}) {
				t.Fatalf("new unknown review lost authority: %+v", selected)
			}
			if count, err := repository.BackfillChangeRecoveryProjections(backfillContext, f.project.OrganizationID, content, 7); err != nil || count != 1 {
				t.Fatalf("late review backfill = %d: %v", count, err)
			}
		}
		assertReviews(first.ID, map[string]string{"reviewer-one": "changes_requested", "reviewer-two": "approved"})
		traceMu.Lock()
		observedStatements, observedSchemaChanges := statements, schemaChanges
		traceMu.Unlock()
		if observedStatements == 0 || observedSchemaChanges != 0 {
			t.Fatalf("review projection SQL: statements=%d schema changes=%d, want observed statements and no schema changes", observedStatements, observedSchemaChanges)
		}
		if _, err := admin.ExecContext(t.Context(), "UPDATE change_evidence SET payload_sha256=repeat('f',64) WHERE organization_id=$1 AND change_id=$2", f.project.OrganizationID, f.change.ID); err == nil {
			t.Fatal("evidence immutability trigger was not restored")
		}
		assertReviews(first.ID, map[string]string{"reviewer-one": "changes_requested", "reviewer-two": "approved"})
	})

	t.Run("claim facts resume from durable unknown rows", func(t *testing.T) {
		f := newChangeFixture(t, newClockedPostgresNativeService(t, WithDeps(Config{}, hubdeps.Deps{RetentionTimeout: frozenRetentionTimeout})))
		version := f.publish(t, "bounded projection", "", true)
		const budget = corestore.MaxPageSize
		reviews := make([]tracker.ChangeReview, 0, budget*3+2)
		for range budget*3 + 2 {
			reviews = append(reviews, tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: version.ID, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-reviewer"}, Decision: "commented", CreatedAt: f.service.config.Clock()})
		}
		appendRetentionReviewEvidence(t, f, reviews)
		content := eventstore.NewContent(f.service.runDetailStore())
		repository := f.service.config.TenantRepository.(*postgres.Repository)
		unknown := func() int {
			t.Helper()
			var count int
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND kind='review' AND NOT review_session_known", f.project.OrganizationID, f.change.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			return count
		}
		if count, err := repository.BackfillChangeRecoveryProjections(t.Context(), f.project.OrganizationID, content, budget); err != nil || count != budget || unknown() != budget*2+2 {
			t.Fatalf("first bounded backfill = %d, unknown=%d: %v", count, unknown(), err)
		}
		for _, want := range []int{budget, budget, 2} {
			if count, err := repository.BackfillChangeRecoveryProjections(t.Context(), f.project.OrganizationID, content, budget); err != nil || count != want {
				t.Fatalf("resumed backfill = %d, want %d: %v", count, want, err)
			}
		}
		if count := unknown(); count != 0 {
			t.Fatalf("unknown=%d, want 0", count)
		}
		first := appendRetentionReviewEvidence(t, f, []tracker.ChangeReview{{ID: hubapi.NewID("review"), VersionID: version.ID, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-reviewer"}, Decision: "approved", CreatedAt: f.service.config.Clock()}})[0]
		ref, err := content.PreparePayload(t.Context(), f.project.OrganizationID, []byte(`{"version_id":42}`))
		if err != nil {
			t.Fatal(err)
		}
		malformed := eventstore.Evidence{OrganizationID: f.project.OrganizationID, ChangeID: f.change.ID, VersionID: new(version.ID), Kind: "review", ActorKind: "human", ActorPrincipalID: "example-reviewer", Decision: "commented", CreatedAt: f.service.config.Clock(), PayloadObjectKey: ref.Key, PayloadSHA256: ref.SHA256, PayloadBytes: ref.CompressedSize, PayloadContentBytes: ref.UncompressedSize, PayloadFormatVersion: 2}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), malformed); return err }); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.BackfillChangeRecoveryProjections(t.Context(), f.project.OrganizationID, content, budget); err == nil {
			t.Fatal("malformed claim fact did not fail")
		}
		var projected bool
		if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT review_session_known FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND sequence=$3", f.project.OrganizationID, f.change.ID, first.Sequence).Scan(&projected); err != nil || !projected {
			t.Fatalf("earlier projection rolled back: projected=%v error=%v", projected, err)
		}
	})

	t.Run("archive error preserves independent compaction", func(t *testing.T) {
		f := newPostgresStorageArchiveFixture(t)
		issue := f.closed(t)
		f.clock.Add(int64(60 * 24 * time.Hour))
		now := f.now()
		at := now.Add(-31 * 24 * time.Hour)
		record := conversationstore.Conversation{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: conversation.NewConversationID(), OwnerPrincipalID: f.scope.Credential.ID, Origin: "user", Title: "Old conversation", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusSettled, CreatedAt: at, UpdatedAt: at, SettledAt: new(at), Execution: conversation.Execution{Status: conversation.ExecutionCompleted, Settled: true, Owner: conversation.Owner{LeaseID: "settled_retention"}}}
		f.write(t, func(w tenantstore.Writer) error {
			var err error
			record, err = w.CreateConversation(t.Context(), record)
			return err
		})
		message := postgresContentTestMessage(t, f, record.ID, conversation.RoleAssistant, "final answer", conversation.DeliveryCompleted)
		for range 2 {
			postgresContentTestDelta(t, f, message, "fragment", 0)
		}
		admin := nativeAttemptTestDatabase(t, f.nativeFixture)
		if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_events SET created_at=$3 WHERE organization_id=$1 AND conversation_id=$2`, f.project.OrganizationID, record.ID, at); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `UPDATE conversations SET created_at=$3,updated_at=$3,last_message_at=$3,settled_at=$3 WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, record.ID, at); err != nil {
			t.Fatal(err)
		}
		verifyHistory := retentionSlotHistory(t, f, issue.WorkItemID)
		archiveErr := errors.New("injected archive failure")
		f.hook = func(request entryauthority.ContentRequest) error {
			if request.Write && request.Reference.SchemaVersion == 2 {
				return archiveErr
			}
			return nil
		}
		if err := f.service.maintainNativeRetention(t.Context(), now); !errors.Is(err, archiveErr) {
			t.Fatalf("retention error=%v", err)
		}
		var deltas int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2 AND type='message.delta'`, f.project.OrganizationID, record.ID).Scan(&deltas); err != nil {
			t.Fatal(err)
		}
		if deltas != 0 {
			t.Fatalf("deltas=%d", deltas)
		}
		verifyHistory()
	})

	t.Run("command receipts and storage accounting", func(t *testing.T) {
		now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
		f := newDefaultNativeFixture(t, WithDeps(Config{Clock: func() time.Time { return now }}, hubdeps.Deps{RetentionTimeout: frozenRetentionTimeout}))
		issue := f.create(t, "receipts")
		credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
		if err != nil {
			t.Fatal(err)
		}
		actor := credential.ID
		admin := usagePostgresAdmin(t, f.service)
		counter := func() int64 {
			var value int64
			if err := admin.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE organization_id=$1 AND name='collaboration_bytes'", f.project.OrganizationID).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		before := counter()
		base := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/projects/" + string(f.project.ID)
		receipts := []struct {
			operation, key string
			age            time.Duration
			kept           bool
		}{
			{"POST " + base + "/landing-barrier", "old", 25 * time.Hour, false},
			{"PATCH " + base + "/work-items/x", "old-patch", 25 * time.Hour, false},
			{"POST " + base + "/landing-barrier", "recent", time.Hour, true},
			{"POST " + base + "/conversations", "create-conversation", 25 * time.Hour, false},
			{"POST " + base + "/conversations/c/link", "link", 25 * time.Hour, false},
			{"mcp update_runner_routing", "mcp", 25 * time.Hour, false},
		}
		content := eventstore.NewContent(f.service.runDetailStore())
		for _, receipt := range receipts {
			at := now.Add(-receipt.age)
			row, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID, ActorID: actor, Operation: receipt.operation, CommandKey: receipt.key, RequestHash: strings.Repeat("a", 64), CreatedAt: at, ResponseReplayUntilNs: at.Add(receiptstore.ReplayRetention).UnixNano()}, []byte(`{"output":"`+fmt.Sprintf("%0512d", 0)+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.PutReceipt(t.Context(), row); return err }); err != nil {
				t.Fatal(err)
			}
		}
		event, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateID: issue.WorkItemID, Type: "run.observed", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: actor}, RecordedAt: now, Data: tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: "attempt"}}}))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
			t.Fatal(err)
		}
		if after := counter(); after != before {
			t.Fatalf("bookkeeping counted against storage: before=%d after=%d", before, after)
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "customer content"})
		requireNativeStatus(t, response, http.StatusOK)
		if counter() <= before {
			t.Fatal("customer comment was not counted")
		}
		if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		for _, receipt := range receipts {
			var expired bool
			if err := admin.QueryRowContext(t.Context(), "SELECT response_expired FROM native_command_states WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND command_key=$4", f.project.OrganizationID, actor, receipt.operation, receipt.key).Scan(&expired); err != nil {
				t.Fatal(err)
			}
			if !expired != receipt.kept {
				t.Fatalf("receipt %s %s body kept=%v", receipt.operation, receipt.key, !expired)
			}
		}
	})
	t.Run("duplicate challenges", func(t *testing.T) {
		now := time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC)
		f := newDefaultNativeFixture(t, WithDeps(Config{Clock: func() time.Time { return now }}, hubdeps.Deps{RetentionTimeout: frozenRetentionTimeout}))
		other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-challenges")
		live := make(map[tracker.ProjectID]string)
		for _, project := range []tracker.NativeProject{f.project, other.project} {
			fixture := f
			if project.ID == other.project.ID {
				fixture = other
			}
			candidate := fixture.create(t, "duplicate candidate")
			for _, test := range []struct {
				name     string
				age      time.Duration
				consumed bool
			}{{"expired", time.Hour + time.Second, false}, {"boundary", time.Hour, false}, {"live", time.Hour - time.Second, false}, {"consumed", 0, true}} {
				token := "dup_" + artifact.Digest([]byte(string(project.ID) + ":" + test.name))[:32]
				at := now.Add(-test.age)
				challenge := corestore.DuplicateChallenge{OrganizationID: project.OrganizationID, ProjectID: project.ID, Token: token, PrincipalID: "principal", RequestID: test.name, ContentHash: strings.Repeat("a", 64), PolicyHash: strings.Repeat("b", 64), CreatedAt: at, ExpiresAt: at.Add(time.Hour), Candidates: []corestore.DuplicateCandidate{{WorkItemID: candidate.WorkItemID, Number: int64(candidate.Number), Revision: int64(candidate.Revision)}}}
				if test.name == "live" {
					live[project.ID] = token
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					owner := w.(corestore.DuplicateChallengeWriter)
					if err := owner.CreateDuplicateChallenge(t.Context(), challenge); err != nil {
						return err
					}
					if test.consumed {
						_, err := owner.ConsumeDuplicateChallenge(t.Context(), challenge, now)
						return err
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		for _, project := range []tracker.NativeProject{f.project, other.project} {
			var count int
			var token string
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*),COALESCE(min(token),'') FROM native_duplicate_challenges WHERE organization_id=$1 AND project_id=$2", project.OrganizationID, project.ID).Scan(&count, &token); err != nil {
				t.Fatal(err)
			}
			if count != 1 || token != live[project.ID] {
				t.Fatalf("retained challenges=%d, token=%s", count, token)
			}
		}

	})
	t.Run("hourly host history", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
		f := newDefaultNativeFixture(t, WithDeps(Config{Clock: func() time.Time { return now }}, hubdeps.Deps{RetentionTimeout: frozenRetentionTimeout}))
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
		r.enroll(t)
		for offset := 0; offset < 744; offset += 24 {
			var summaries []hostmetrics.Summary
			for index := offset; index < min(offset+24, 744); index++ {
				summaries = append(summaries, testHostSummary(now.Add(-time.Duration(index)*time.Hour)))
			}
			reportedAt := now.Add(-time.Duration(offset) * time.Hour)
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.AppendRunnerHostMetricSegments(t.Context(), r.binding.RunnerID, summaries, reportedAt)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		var seeded int
		if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(DISTINCT hour) FROM runner_host_metric_segments WHERE organization_id=$1 AND runner_id=$2", f.project.OrganizationID, r.binding.RunnerID).Scan(&seeded); err != nil || seeded != 744 {
			t.Fatalf("seeded host history = %d, want 744: %v", seeded, err)
		}
		if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(DISTINCT hour) FROM runner_host_metric_segments WHERE organization_id=$1 AND runner_id=$2", f.project.OrganizationID, r.binding.RunnerID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 720 {
			t.Fatalf("retained %d hours, want 720", count)
		}
		stale := testHostSummary(now.Add(-720 * time.Hour))
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": r.redemption.DisplayName, "capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation, "host_metrics": []hostmetrics.Summary{stale}})
		requireNativeStatus(t, response, http.StatusOK)
		if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(DISTINCT hour) FROM runner_host_metric_segments WHERE organization_id=$1 AND runner_id=$2", f.project.OrganizationID, r.binding.RunnerID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 720 {
			t.Fatalf("stale retry recreated history: %d hours", count)
		}
		history, err := f.service.readRunnerHostHistory(t.Context(), f.project.OrganizationID, r.binding.RunnerID, now.UTC().Truncate(time.Hour).Add(-719*time.Hour), now.UTC().Truncate(time.Hour).Add(time.Hour))
		if err != nil || len(history) != 720 {
			t.Fatalf("bounded history=%d error=%v", len(history), err)
		}
	})

	t.Parallel()
	for _, test := range []struct {
		name, state                                                                                                     string
		age                                                                                                             int
		lease, expiredLease, openChange, landedChange, never, restore, typedRestore, typedOpen, terminalMove, noHistory bool
		period                                                                                                          int
		archived                                                                                                        bool
	}{
		{name: "old Done", state: "Done", age: 31, archived: true},
		{name: "recent Done", state: "Done", age: 29},
		{name: "Done boundary", state: "Done", age: 30},
		{name: "old Cancelled", state: "Cancelled", age: 8, archived: true},
		{name: "recent Cancelled", state: "Cancelled", age: 6},
		{name: "live lease", state: "Done", age: 31, lease: true},
		{name: "expired lease", state: "Done", age: 31, lease: true, expiredLease: true, archived: true},
		{name: "open Change", state: "Done", age: 31, openChange: true},
		{name: "landed Change", state: "Done", age: 31, openChange: true, landedChange: true, archived: true},
		{name: "Never completed", state: "Done", age: 100, never: true},
		{name: "Never cancelled", state: "Cancelled", age: 100, never: true},
		{name: "typed restore resets clock", state: "Done", age: 100, typedRestore: true},
		{name: "typed pending Change", state: "Done", age: 31, openChange: true, landedChange: true, typedOpen: true},
		{name: "restore resets clock", state: "Done", age: 100, restore: true},
		{name: "custom terminal", state: "Shipped", age: 31, archived: true},
		{name: "terminal move preserves clock", state: "Shipped", age: 31, terminalMove: true, archived: true},
		{name: "no terminal history", state: "Done", age: 100, noHistory: true},
		{name: "nonterminal", state: "Todo", age: 100},
		{name: "short completed period", state: "Done", age: 8, period: 7, archived: true},
		{name: "long cancelled period", state: "Cancelled", age: 8, period: 14},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPostgresStorageArchiveFixture(t)
			started := f.now()
			f.write(t, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				if test.state == "Shipped" || test.state == "Cancelled" {
					project.States = append(project.States, tracker.NativeState{Name: test.state, Terminal: true})
					for i := range project.States {
						if project.States[i].Name == "Todo" || project.States[i].Name == "Done" {
							project.States[i].Transitions = append(project.States[i].Transitions, test.state)
						}
					}
				}
				if test.never || test.period != 0 {
					var days *int64
					if !test.never {
						days = new(int64(test.period))
					}
					if test.state == "Cancelled" {
						project.Settings.ArchiveCancelledAfterDays = days
					} else {
						project.Settings.ArchiveCompletedAfterDays = days
					}
				}
				_, err = w.UpdateProjectRecord(t.Context(), project)
				return err
			})
			issue := f.create(t, "retention")
			if test.state != "Todo" {
				state := test.state
				if test.terminalMove {
					state = "Done"
				}
				issue = f.transition(t, issue.WorkItemID, state)
			}
			f.clock.Store(started.Add(time.Duration(test.age) * 24 * time.Hour).UnixNano())
			if test.terminalMove {
				f.clock.Add(-int64(time.Hour))
				issue = f.transition(t, issue.WorkItemID, test.state)
				f.clock.Add(int64(time.Hour))
			}
			now := f.now()
			admin := nativeAttemptTestDatabase(t, f.nativeFixture)
			if test.noHistory {
				if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET terminal_entered_at=NULL WHERE organization_id=$1 AND native_id=$2`, f.project.OrganizationID, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if test.restore || test.typedRestore {
				f.clock.Add(-int64(2 * time.Hour))
				for _, archived := range []bool{true, false} {
					operation := "restore"
					if archived {
						operation = "archive"
					}
					request := archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: "retention-" + operation}, ExpectedRevision: issue.Revision}
					if test.typedRestore {
						raw, err := f.service.setNativeArchiveCommand(t.Context(), f.scope, string(issue.WorkItemID), request, archived)
						if err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(raw, &issue); err != nil {
							t.Fatal(err)
						}
					} else {
						response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/"+operation, f.token, request)
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &issue)
					}
					f.clock.Add(int64(time.Hour))
				}
			}
			if test.lease {
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO machines (organization_id,id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES ($1,'machine_retention','example','example',1,'test',$2,$2,$2)`, f.project.OrganizationID, now); err != nil {
					t.Fatal(err)
				}
				expires := now.Add(time.Hour)
				if test.expiredLease {
					expires = now.Add(-time.Minute)
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO leases (organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at) SELECT organization_id,'lease_retention',id,'machine_retention','session_retention',revision,$3,$3,$4 FROM issues WHERE organization_id=$1 AND native_id=$2`, f.project.OrganizationID, issue.WorkItemID, now.Add(-time.Hour), expires); err != nil {
					t.Fatal(err)
				}
			}
			var change tracker.ChangeRequest
			if test.openChange {
				change = seedRetentionChange(t, f, issue.WorkItemID, test.landedChange, test.typedOpen)
			}
			if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			got := readWorkItem(t, f.nativeFixture, issue.WorkItemID, "")
			if got.Archived != test.archived {
				t.Fatalf("archived=%v, want %v", got.Archived, test.archived)
			}
			if !test.archived && got.Revision != issue.Revision {
				t.Fatalf("skipped issue changed revision: %d", got.Revision)
			}
			if test.archived {
				if got.Revision != issue.Revision+1 || got.State != issue.State || got.Body != issue.Body {
					t.Fatalf("archive changed content: %+v", got)
				}
				var stored eventstore.Event
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					events, err := r.HistoryEvents(t.Context(), f.project.ID, issue.WorkItemID, 0, 32)
					if err != nil {
						return err
					}
					if len(events) == 0 {
						t.Fatal("archive event missing")
					}
					stored = events[len(events)-1]
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				event, err := eventstore.NewContent(f.service.runDetailStore()).Event(t.Context(), stored)
				if err != nil {
					t.Fatal(err)
				}
				days := 30
				if test.state == "Cancelled" {
					days = 7
				}
				if test.period != 0 {
					days = test.period
				}
				if event.Data.Operation != "archive" || event.Data.ReasonDetail != fmt.Sprintf("Archived automatically after %d days", days) || event.Actor.Kind != "integration" || event.Actor.PrincipalID != "auto_archive" {
					t.Fatalf("archive event=%+v actor=%+v", event.Data, event.Actor)
				}
				if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
					t.Fatal(err)
				}
				if repeated := readWorkItem(t, f.nativeFixture, issue.WorkItemID, ""); repeated.Revision != got.Revision {
					t.Fatal("sweep archived twice")
				}
			}
			if test.restore || test.lease && !test.expiredLease || test.openChange && !test.landedChange {
				if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET released_at=$2 WHERE organization_id=$1 AND released_at IS NULL`, f.project.OrganizationID, now); err != nil {
					t.Fatal(err)
				}
				if test.openChange {
					landRetentionChange(t, f, change)
				}
				retryAt := now.Add(time.Minute)
				if test.restore {
					retryAt = now.Add(31 * 24 * time.Hour)
				}
				f.clock.Store(retryAt.UnixNano())
				if err := f.service.maintainNativeRetention(t.Context(), retryAt); err != nil {
					t.Fatal(err)
				}
				if !readWorkItem(t, f.nativeFixture, issue.WorkItemID, "").Archived {
					t.Fatal("eligible retry did not archive")
				}
			}
		})
	}
}

func TestProjectArchivePeriods(t *testing.T) {
	t.Parallel()
	f := newPostgresStorageArchiveFixture(t)
	read := func() ProjectIntegration {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/integration", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var current ProjectIntegration
		decodeHubResponse(t, response, &current)
		return current
	}
	current := read()
	if current.ArchiveCompletedAfterDays == nil || *current.ArchiveCompletedAfterDays != 30 || current.ArchiveCancelledAfterDays == nil || *current.ArchiveCancelledAfterDays != 7 {
		t.Fatalf("defaults=%+v", current)
	}
	for index, test := range []struct {
		name                         string
		completed, cancelled         any
		sendCompleted, sendCancelled bool
		status                       int
		wantCompleted, wantCancelled int
	}{
		{name: "set", completed: 14, sendCompleted: true, status: 200, wantCompleted: 14, wantCancelled: 7},
		{name: "never", cancelled: nil, sendCancelled: true, status: 200, wantCompleted: 14},
		{name: "omit preserves", status: 200, wantCompleted: 14},
		{name: "invalid days", completed: 8, sendCompleted: true, status: 422, wantCompleted: 14},
		{name: "string", completed: "30", sendCompleted: true, status: 422, wantCompleted: 14},
		{name: "zero", completed: 0, sendCompleted: true, status: 422, wantCompleted: 14},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := map[string]any{"idempotency_key": fmt.Sprint("period-", index), "expected_revision": fmt.Sprint(current.Revision), "intake": current.Intake, "projection": current.Projection, "repository_enabled": current.RepositoryEnabled}
			if test.sendCompleted {
				payload["archive_completed_after_days"] = test.completed
			}
			if test.sendCancelled {
				payload["archive_cancelled_after_days"] = test.cancelled
			}
			response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload)
			requireNativeStatus(t, response, test.status)
			current = read()
			if current.ArchiveCompletedAfterDays == nil || *current.ArchiveCompletedAfterDays != test.wantCompleted {
				t.Fatalf("completed=%v", current.ArchiveCompletedAfterDays)
			}
			if test.wantCancelled == 0 && current.ArchiveCancelledAfterDays != nil || test.wantCancelled != 0 && (current.ArchiveCancelledAfterDays == nil || *current.ArchiveCancelledAfterDays != test.wantCancelled) {
				t.Fatalf("cancelled=%v", current.ArchiveCancelledAfterDays)
			}
		})
	}
	issue := f.create(t, "settings during work")
	admin := nativeAttemptTestDatabase(t, f.nativeFixture)
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO machines (organization_id,id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES ($1,'machine_periods','example','example',1,'test',$2,$2,$2)`, f.project.OrganizationID, f.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO leases (organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at) SELECT organization_id,'lease_periods',id,'machine_periods','session_periods',revision,$3,$3,clock_timestamp()+interval '1 hour' FROM issues WHERE organization_id=$1 AND native_id=$2`, f.project.OrganizationID, issue.WorkItemID, f.now()); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"idempotency_key": "live-settings", "expected_revision": fmt.Sprint(current.Revision), "intake": current.Intake, "projection": current.Projection, "repository_enabled": current.RepositoryEnabled, "archive_completed_after_days": 60}
	response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload)
	requireNativeStatus(t, response, http.StatusOK)
	var updated ProjectIntegration
	decodeHubResponse(t, response, &updated)
	if updated.ArchiveCompletedAfterDays == nil || *updated.ArchiveCompletedAfterDays != 60 {
		t.Fatalf("live setting not persisted: %+v", updated)
	}
	payload["idempotency_key"] = "live-workflow"
	payload["expected_revision"] = fmt.Sprint(updated.Revision)
	payload["states"] = f.project.States
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload), http.StatusUnprocessableEntity)

	f.write(t, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Settings.CheckoutRepository = "owner/checkout-example"
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	})
	checkout := read()
	delete(payload, "states")
	for _, test := range []struct {
		name   string
		intake string
		days   int
	}{
		{"checkout-only-period", "disabled", 60},
		{"checkout-only-omitted-intake", "", 14},
	} {
		payload["idempotency_key"] = test.name
		payload["expected_revision"] = fmt.Sprint(checkout.Revision)
		payload["archive_completed_after_days"] = test.days
		if test.intake == "" {
			delete(payload, "intake")
		} else {
			payload["intake"] = test.intake
		}
		response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, payload)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &checkout)
		if checkout.Intake != "automatic" || checkout.ArchiveCompletedAfterDays == nil || *checkout.ArchiveCompletedAfterDays != test.days {
			t.Fatalf("archive-only save = %+v", checkout)
		}
		var intake string
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			project, err := r.ProjectRecord(t.Context(), f.project.ID)
			intake = project.Settings.Intake
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if intake != "disabled" {
			t.Fatalf("archive-only save changed integration authority: %s", intake)
		}
	}

}

func TestNativeStorageArchiveRetention(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, state string
		age         time.Duration
		want        bool
	}{
		{"done before", "Done", 72*time.Hour - time.Second, false},
		{"done boundary", "Done", 72 * time.Hour, true},
		{"done after", "Done", 72*time.Hour + time.Second, true},
		{"cancelled before", "Cancelled", 72*time.Hour - time.Second, false},
		{"cancelled boundary", "Cancelled", 72 * time.Hour, true},
		{"cancelled after", "Cancelled", 73 * time.Hour, true},
		{"open", "Todo", 96 * time.Hour, false},
		{"not started", "Backlog", 96 * time.Hour, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPostgresStorageArchiveFixture(t)
			now := f.now().Add(test.age)
			issue := f.create(t, "archive cutoff")
			if test.state != "Todo" {
				f.write(t, func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), f.project.ID)
					if err != nil {
						return err
					}
					if !slices.ContainsFunc(project.States, func(state tracker.NativeState) bool { return state.Name == test.state }) {
						project.States = append(project.States, tracker.NativeState{Name: test.state, Terminal: test.state == "Cancelled"})
					}
					for index := range project.States {
						state := &project.States[index]
						if state.Name == "Todo" && !slices.Contains(state.Transitions, test.state) {
							state.Transitions = append(state.Transitions, test.state)
						}
					}
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				})
				issue = f.transition(t, issue.WorkItemID, test.state)
			}
			f.clock.Store(now.UnixNano())
			if err := f.service.archiveClosedNativeIssues(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			var pointer attachmentstore.StorageArchivePointer
			err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				pointer, err = r.(attachmentstore.StorageArchiveReader).StorageArchivePointer(t.Context(), f.project.ID, issue.WorkItemID)
				return err
			})
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				t.Fatal(err)
			}
			if (err == nil) != test.want {
				t.Fatalf("archive pointer=%+v error=%v want=%v", pointer, err, test.want)
			}
			got := readWorkItem(t, f.nativeFixture, issue.WorkItemID, "")
			if got.Archived || got.Revision != issue.Revision {
				t.Fatalf("storage archival changed board issue: %+v", got)
			}
			if test.want {
				ref := pointer.Root.Reference
				f.mu.Lock()
				data := slices.Clone(f.objects[ref.Key])
				f.mu.Unlock()
				if ref.Validate(string(f.project.OrganizationID)) != nil || artifact.Digest(data) != ref.SHA256 || int64(len(data)) != ref.CompressedSize || int64(len(archiveTestBytes(t, data))) != ref.UncompressedSize {
					t.Fatalf("invalid root pointer: %+v", ref)
				}
				if err := f.service.archiveClosedNativeIssues(t.Context(), now); err != nil {
					t.Fatal(err)
				}
				if count := len(f.archiveObjects(t)); count != 1 {
					t.Fatalf("repeat publication created %d objects", count)
				}
			}
		})
	}
}

func TestNativeRetentionCommitsEachIssue(t *testing.T) {
	t.Parallel()
	f := newPostgresStorageArchiveFixture(t)
	for range 3 {
		f.closed(t)
	}
	f.clock.Add(int64(31 * 24 * time.Hour))
	admin := nativeAttemptTestDatabase(t, f.nativeFixture)
	if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION interrupt_archive() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.archived AND (SELECT count(*) FROM issues WHERE archived)=1 THEN RAISE EXCEPTION 'interrupted archive'; END IF; RETURN NEW; END $$; CREATE TRIGGER interrupt_archive BEFORE UPDATE OF archived ON issues FOR EACH ROW EXECUTE FUNCTION interrupt_archive()`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.expireNativeRecords(t.Context(), f.now()); err == nil {
		t.Fatal("expected archive interruption")
	}
	for _, want := range []int{1, 3} {
		if want == 3 {
			if _, err := admin.ExecContext(t.Context(), `DROP TRIGGER interrupt_archive ON issues`); err != nil {
				t.Fatal(err)
			}
			if err := f.service.expireNativeRecords(t.Context(), f.now()); err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM issues WHERE organization_id=$1 AND archived`, f.project.OrganizationID).Scan(&count); err != nil || count != want {
			t.Fatalf("archived=%d want=%d err=%v", count, want, err)
		}
	}
}

func appendRetentionReviewEvidence(t *testing.T, f changeFixture, reviews []tracker.ChangeReview) []eventstore.Evidence {
	t.Helper()
	content := eventstore.NewContent(f.service.runDetailStore())
	rows := make([]eventstore.Evidence, 0, len(reviews))
	for _, review := range reviews {
		raw, err := json.Marshal(review)
		if err != nil {
			t.Fatal(err)
		}
		row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: f.project.OrganizationID, ChangeID: f.change.ID, Kind: "review"}, raw)
		if err != nil {
			t.Fatal(err)
		}
		row.ReviewKind, row.ReviewSessionKnown = "", false
		rows = append(rows, row)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		for index, row := range rows {
			stored, err := w.AppendEvidence(t.Context(), row)
			if err != nil {
				return err
			}
			rows[index] = stored
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

func readRetentionReviewEvidence(t *testing.T, f changeFixture, version string) []tracker.ChangeReview {
	t.Helper()
	var rows []eventstore.Evidence
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		change, err := r.Change(t.Context(), f.change.ID)
		if err != nil {
			return err
		}
		rows, _, err = r.(eventstore.ChangeRecoveryReader).ChangeSummaryEvidence(t.Context(), f.change.ID, version, change.EvidenceSequence, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(f.service.runDetailStore())
	var reviews []tracker.ChangeReview
	for _, row := range rows {
		if row.Kind != "review" {
			continue
		}
		raw, err := content.Evidence(t.Context(), row)
		if err != nil {
			t.Fatal(err)
		}
		review, err := eventstore.DecodeChangeReview(row, raw)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	return reviews
}
