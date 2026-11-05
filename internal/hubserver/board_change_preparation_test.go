package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardChangeCountingRepository struct {
	tenantstore.Repository
	reads  atomic.Int64
	scalar bool
}
type boardChangeScalarReader struct{ tenantstore.Reader }

func (r *boardChangeCountingRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.reads.Add(1)
	return r.Repository.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error {
		if r.scalar {
			return read(boardChangeScalarReader{reader})
		}
		return read(reader)
	})
}
func (r *boardChangeCountingRepository) ReadTenantSnapshot(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.reads.Add(1)
	return r.Repository.(tenantstore.SnapshotRepository).ReadTenantSnapshot(ctx, org, workload, read)
}

type boardDetailBatchCounter struct {
	attachment.DetailStore
	batches atomic.Int64
	scalar  atomic.Int64
}

func (d *boardDetailBatchCounter) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	d.scalar.Add(1)
	return d.DetailStore.ReadDetail(ctx, organization, ref)
}
func (d *boardDetailBatchCounter) ReadDetails(ctx context.Context, organization string, refs []attachment.ArchiveReference) ([]attachment.DetailReadResult, error) {
	d.batches.Add(1)
	return attachment.ReadDetailBatch(ctx, organization, refs, d.DetailStore.ReadDetail)
}
func TestBoardChangePageBatchesTwoHundredArchivedAndCurrentCards(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	counted := &boardChangeCountingRepository{Repository: repository}
	details := &memoryRunDetails{}
	batchDetails := &boardDetailBatchCounter{DetailStore: details}
	service := &Service{database: &database{postgres: repository}, config: Config{TenantRepository: counted, AccessRepository: counted, SecretKeys: secretTestKeys(t, "1", "1"), RunDetails: batchDetails, now: func() time.Time { return now }}}
	content := eventstore.NewContent(details)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		service.localOrganization = string(org)
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: org, Name: string(org), Slug: string(org)})
		var project projectstore.Record
		descriptor := hubTestPolicy()
		policy, err := content.PrepareProjectPolicy(t.Context(), org, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		policy.ProjectID = "example"
		policy.ApprovedBy = "test_hub_admin"
		policy.ApprovedAt = now
		envelope, err := service.config.SecretKeys.Seal(bytes.Repeat([]byte{1}, 32), accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			var err error
			project, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example", Profile: "native"}, States: nativeFixtureStates()})
			if err != nil {
				return err
			}
			if err := w.ApproveProjectPolicy(t.Context(), policy, ""); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		for index := range 200 {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%03d", index))
			changeID := fmt.Sprintf("change_%03d", index)
			versionID := fmt.Sprintf("version_%03d", index)
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: id, Number: index + 1, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "test_hub_admin"}, Archived: index%2 == 0, CreatedAt: now, UpdatedAt: now}
			if index == 2 {
				issue.Actor.PrincipalID = "other"
			}
			raw, _ := json.Marshal(issue)
			collaboration, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			change := tracker.ChangeRequest{ID: changeID, OrganizationID: org, ProjectID: "example", WorkItemID: id, LinkedIssues: []tracker.NativeWorkItemID{id}, Title: "Example", Revision: 1, CurrentVersion: versionID, CreatedAt: now, UpdatedAt: now}
			raw, _ = json.Marshal(change)
			changeRow, err := content.PrepareChange(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			input := changeTestInput()
			input.PolicyID = descriptor.ID
			version := tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, ChangeVersionInput: input, Policy: descriptor, Actor: issue.Actor, CreatedAt: now}
			raw, _ = json.Marshal(version)
			versionRow, err := content.PrepareVersion(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			priorReview := tracker.ChangeReview{ID: fmt.Sprintf("review_%03d", index), Decision: "approved", Body: "Previously approved", Actor: issue.Actor, CreatedAt: now.Add(-time.Minute)}
			reviewRaw, err := json.Marshal(priorReview)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: org, ChangeID: changeID, Kind: "review"}, reviewRaw)
			if err != nil {
				t.Fatal(err)
			}
			card := tracker.NativeBoardCard{Issue: issue, Change: &tracker.NativeBoardChange{ID: changeID, Title: "Example"}}
			payload, err := viewstore.NewContent(details).PrepareBoard(t.Context(), card)
			if err != nil {
				t.Fatal(err)
			}
			board := viewstore.BoardRecord{OrganizationID: org, ProjectID: "example", WorkItemID: id, SourceRevision: 1, SourceProjectRevision: project.Core.ContentSettingsRevision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("example#%03d", index)}, Archived: issue.Archived, ActorPrincipalID: issue.Actor.PrincipalID, CreatedAt: now, UpdatedAt: now, Payload: payload}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: id, Number: int64(index + 1), Title: "Example", Archived: issue.Archived, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), collaboration); err != nil {
					return err
				}
				if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
					return err
				}
				if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
					return err
				}
				if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: id, ChangeID: changeID, CreatedAt: now}); err != nil {
					return err
				}
				if _, err := w.AppendEvidence(t.Context(), evidence); err != nil {
					return err
				}
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: "example", WorkItemID: id, ExpectedRevision: 1, ExpectedProjectRevision: project.Core.ContentSettingsRevision, Current: &board})
			}); err != nil {
				t.Fatal(err)
			}
		}
		newer := tracker.ChangeRequest{ID: "change_newer", OrganizationID: org, ProjectID: "example", WorkItemID: "wi_000", LinkedIssues: []tracker.NativeWorkItemID{"wi_000"}, Title: "Newer draft", Revision: 1, CreatedAt: now.Add(time.Minute), UpdatedAt: now.Add(time.Minute)}
		raw, _ := json.Marshal(newer)
		newerRow, err := content.PrepareChange(t.Context(), org, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateChange(t.Context(), newerRow); err != nil {
				return err
			}
			return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_000", ChangeID: newer.ID, CreatedAt: newer.CreatedAt})
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: "wi_002", ChangeID: "change_001", CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: org, project: "example", credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
		counted.scalar = true
		counted.reads.Store(0)
		before, err := service.readBoardRecords(t.Context(), scope, url.Values{"limit": {"200"}, "archived": {"all"}, "include": {"board"}})
		if err != nil {
			t.Fatal(err)
		}
		scalarReads := counted.reads.Load()
		batchDetails.batches.Store(0)
		batchDetails.scalar.Store(0)
		counted.scalar = false
		counted.reads.Store(0)
		timing := &requestTiming{start: time.Now()}
		after, err := service.readBoardRecords(withRequestTiming(t.Context(), timing), scope, url.Values{"limit": {"200"}, "archived": {"all"}, "include": {"board"}})
		if err != nil {
			t.Fatal(err)
		}
		batchReads := counted.reads.Load()
		phases := map[string]bool{}
		for _, span := range timing.namedSpans() {
			phases[span.name] = true
		}
		for _, phase := range []string{"board_cursor", "board_select", "board_payloads", "board_relationships", "board_change_rows", "board_change_payloads", "board_change_policy", "board_hydrate"} {
			if !phases[phase] {
				t.Errorf("board timing omitted phase %s", phase)
			}
		}
		t.Logf("board phases: %+v", timing.namedSpans())
		if batches, scalar := batchDetails.batches.Load(), batchDetails.scalar.Load(); batches < 3 || batches > 5 || scalar != 0 {
			t.Fatalf("200-card allocation lookups: batches=%d scalar=%d", batches, scalar)
		}
		if len(before.Cards) != 200 || len(after.Cards) != 200 {
			t.Fatalf("page sizes before%d after%d", len(before.Cards), len(after.Cards))
		}
		archived := 0
		for index, card := range after.Cards {
			if card.Issue.OrganizationID != org || card.Issue.WorkItemID != before.Cards[index].Issue.WorkItemID || card.Change == nil || card.Change.Status != before.Cards[index].Change.Status {
				t.Fatalf("changed scoped card%d", index)
			}
			if card.Issue.Archived {
				archived++
			}
		}
		if archived != 100 || scalarReads < 800 || batchReads > 10 {
			t.Fatalf("archived%d scalarUOW%d batchUOW%d", archived, scalarReads, batchReads)
		}
		t.Logf("tenant=%s cards=200 scalar_uows=%d batch_uows=%d", org, scalarReads, batchReads)
		reporter := scope
		reporter.credential.HostedRole = "reporter"
		reporterPage, err := service.readBoardRecords(t.Context(), reporter, url.Values{"limit": {"200"}, "archived": {"all"}, "include": {"board"}})
		if err != nil || len(reporterPage.Cards) != 199 {
			t.Fatalf("reporter cards%d error%v", len(reporterPage.Cards), err)
		}
		found := false
		for _, card := range reporterPage.Cards {
			if card.Issue.WorkItemID == "wi_002" {
				t.Fatal("reporter received another principal issue")
			}
			if card.Issue.WorkItemID == "wi_001" {
				found = true
				if card.Change == nil || card.Change.ID != "change_001" {
					t.Fatal("hidden secondary link suppressed the visible primary change")
				}
			}
		}
		if !found {
			t.Fatal("reporter lost visible primary issue")
		}

	}
}

func TestBoardSummaryReadsStaleReviewPolicyWithinTenant(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	details := &memoryRunDetails{}
	var previewLogs bytes.Buffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(previewLogs.String())
		}
	})
	service := &Service{database: &database{postgres: repository}, echo: echo.New(), secretRewrapReady: make(chan struct{}), config: Config{TenantRepository: repository, AccessRepository: repository, Logger: slog.New(slog.NewTextHandler(&previewLogs, nil)), SecretKeys: secretTestKeys(t, "1", "1"), RunDetails: details, Clock: func() time.Time { return now }}.normalized()}
	service.echo.POST(nativeBase+"/claims/preview", service.previewProviderCandidates, service.requireNativeScope(apiScopeWorker))
	service.echo.GET(nativeBase+"/work-items/:item", service.getNativeIssue, service.requireNativeScope(apiScopeWorker, apiScopeOperator))
	if service.database.db != nil {
		t.Fatal("preview fixture has a legacy database")
	}
	content := eventstore.NewContent(details)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		service.localOrganization = string(org)
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: org, Name: string(org), Slug: string(org)})
		var project projectstore.Record
		descriptor := hubTestPolicy()
		policy, err := content.PrepareProjectPolicy(t.Context(), org, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		policy.ProjectID = "example"
		policy.ApprovedBy = "test_hub_admin"
		policy.ApprovedAt = now
		envelope, err := service.config.SecretKeys.Seal(bytes.Repeat([]byte{1}, 32), accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			var err error
			project, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example", Profile: "native"}, States: append(nativeFixtureStates(), tracker.NativeState{Name: "Merging", Dispatchable: true})})
			if err != nil {
				return err
			}
			if err := w.ApproveProjectPolicy(t.Context(), policy, ""); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		for index := range 2 {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%03d", index))
			changeID := fmt.Sprintf("change_%03d", index)
			versionID := fmt.Sprintf("version_%03d", index)
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: id, Number: index + 1, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "test_hub_admin"}, Archived: false, CreatedAt: now, UpdatedAt: now}
			raw, _ := json.Marshal(issue)
			collaboration, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			change := tracker.ChangeRequest{ID: changeID, OrganizationID: org, ProjectID: "example", WorkItemID: id, LinkedIssues: []tracker.NativeWorkItemID{id}, Title: "Example", Revision: 1, CurrentVersion: versionID, CreatedAt: now, UpdatedAt: now}
			raw, _ = json.Marshal(change)
			changeRow, err := content.PrepareChange(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			input := changeTestInput()
			input.PolicyID = descriptor.ID
			version := tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, ChangeVersionInput: input, Policy: descriptor, ReviewPolicy: defaultChangeReviewPolicy(descriptor), Actor: issue.Actor, CreatedAt: now}
			raw, _ = json.Marshal(version)
			versionRow, err := content.PrepareVersion(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			card := tracker.NativeBoardCard{Issue: issue, Change: &tracker.NativeBoardChange{ID: changeID, Title: "Example"}}
			payload, err := viewstore.NewContent(details).PrepareBoard(t.Context(), card)
			if err != nil {
				t.Fatal(err)
			}
			board := viewstore.BoardRecord{OrganizationID: org, ProjectID: "example", WorkItemID: id, SourceRevision: 1, SourceProjectRevision: project.Core.ContentSettingsRevision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("example#%03d", index)}, Archived: issue.Archived, ActorPrincipalID: issue.Actor.PrincipalID, CreatedAt: now, UpdatedAt: now, Payload: payload}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "example", NativeID: id, Number: int64(index + 1), Title: "Example", Archived: issue.Archived, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, WorkflowStateName: &issue.State}); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), collaboration); err != nil {
					return err
				}
				if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
					return err
				}
				if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
					return err
				}
				if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "example", WorkItemID: id, ChangeID: changeID, CreatedAt: now}); err != nil {
					return err
				}
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: "example", WorkItemID: id, ExpectedRevision: 1, ExpectedProjectRevision: project.Core.ContentSettingsRevision, Current: &board})
			}); err != nil {
				t.Fatal(err)
			}
		}
		scope := nativeScope{organization: org, project: "example", credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
		before, err := service.readBoardRecords(t.Context(), scope, url.Values{"limit": {"200"}, "include": {"board"}})
		if err != nil || len(before.Cards) != 2 {
			t.Fatalf("current-policy board cards=%d error=%v", len(before.Cards), err)
		}
		for _, card := range before.Cards {
			if card.Change == nil || card.Change.Status == "stale_policy" {
				t.Fatalf("current policy incorrectly stale: %+v", card.Change)
			}
		}

		rules := defaultChangeReviewPolicy(descriptor)
		review, err := content.PrepareReviewPolicy(t.Context(), eventstore.ReviewPolicy{OrganizationID: org, ProjectID: "example", PolicyRevisionID: descriptor.ID, Revision: 1, UpdatedBy: "test_hub_admin", UpdatedAt: now}, rules)
		if err != nil {
			t.Fatal(err)
		}
		nextDescriptor := descriptor
		nextDescriptor.Gates.Validator = true
		nextDescriptor = nextDescriptor.WithID()
		nextPolicy, err := content.PrepareProjectPolicy(t.Context(), org, nextDescriptor)
		if err != nil {
			t.Fatal(err)
		}
		nextPolicy.ProjectID, nextPolicy.ApprovedBy, nextPolicy.ApprovedAt = "example", "test_hub_admin", now
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.SetReviewPolicy(t.Context(), review, ""); err != nil {
				return err
			}
			return w.ApproveProjectPolicy(t.Context(), nextPolicy, descriptor.ID)
		}); err != nil {
			t.Fatal(err)
		}
		stale, err := service.readBoardRecords(t.Context(), scope, url.Values{"limit": {"200"}, "archived": {"all"}, "include": {"board"}})
		if err != nil {
			t.Fatalf("stale review reference prevents scoped board read: %v", err)
		}
		if len(stale.Cards) != 2 {
			t.Fatalf("stale-policy cards=%d", len(stale.Cards))
		}
		for _, card := range stale.Cards {
			if card.Issue.OrganizationID != org || card.Change == nil || card.Change.Status != "stale_policy" {
				t.Fatalf("stale policy summary lost tenant or fence: %+v", card.Change)
			}
		}
		listed, err := service.readIssuesProjection(t.Context(), scope, url.Values{"limit": {"200"}, "state": {"Todo"}, "include": {"summary"}}, true)
		if err != nil || len(listed.Items) != 2 || listed.Total != 2 {
			t.Fatalf("Todo summary list items=%d total=%d error=%v", len(listed.Items), listed.Total, err)
		}
		for _, issue := range listed.Items {
			if issue.OrganizationID != org || issue.ProjectID != scope.project || issue.State != "Todo" {
				t.Fatalf("Todo summary list escaped scope: %+v", issue.NativeReference)
			}
		}
		summary, err := service.selectedCurrentChangeSummary(t.Context(), scope, "wi_001", "change_001", now)
		if err != nil || summary.Status != "stale_policy" {
			t.Fatalf("scalar stale-policy summary=%+v error=%v", summary, err)
		}
		detail, err := service.selectedChangeDetail(t.Context(), scope, "wi_001", "change_001", now)
		if err != nil || detail.Change.OrganizationID != org || detail.Summary.Status != "stale_policy" {
			t.Fatalf("stale-policy detail=%+v error=%v", detail.Summary, err)
		}
		bounded, err := service.readBoundedChangeRecord(t.Context(), scope, operatortool.ChangeArguments{ItemID: "wi_001", ChangeID: "change_001"}, operatortool.ChangeResult{})
		if err != nil || bounded.Detail == nil || bounded.Detail.Change.OrganizationID != org || bounded.Detail.Summary.Status != "stale_policy" {
			t.Fatalf("bounded stale-policy read=%+v error=%v", bounded.Detail, err)
		}
		authority := tenantChangeVersionAuthority{repository: repository, content: content}
		for _, check := range []struct {
			name string
			read func() error
		}{
			{name: "publication preparation", read: func() error {
				_, err := authority.Prepare(t.Context(), scope, detail.Change, tracker.PublishChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{PolicyID: nextDescriptor.ID}}, now)
				return err
			}},
			{name: "publication authorization", read: func() error {
				return repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return authority.Authorize(t.Context(), w, scope, detail.Change, tracker.PublishChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{PolicyID: nextDescriptor.ID}}, ChangeVersionApproval{Policy: nextDescriptor, ReviewPolicy: rules}, now)
				})
			}},
			{name: "review approval policy", read: func() error {
				_, err := service.currentChangePolicyRows(t.Context(), scope)
				return err
			}},
			{name: "landing transaction policy", read: func() error {
				return repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := readCurrentChangePolicyRows(t.Context(), w, scope)
					return err
				})
			}},
		} {
			if err := check.read(); !errors.Is(err, corestore.ErrTenantMismatch) {
				t.Fatalf("%s: stale %s policy accepted: %v", org, check.name, err)
			}
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			foreign := scope
			foreign.organization = "org_first"
			if org == foreign.organization {
				foreign.organization = "org_second"
			}
			_, err := readChangeSummaryPolicyRows(t.Context(), r, foreign)
			return err
		}); !errors.Is(err, corestore.ErrTenantMismatch) {
			t.Fatalf("%s: cross-tenant summary policy accepted: %v", org, err)
		}
		for index := range 2 {
			id := fmt.Sprintf("wi_%03d", index)
			path := "/api/v2/organizations/" + string(org) + "/projects/example/work-items/" + id + "?view=recovery&projection=bounded"
			response := performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
			if response.Code != http.StatusOK {
				t.Errorf("%s/%s: stale review prevents recovery: status=%d body=%s", org, id, response.Code, response.Body.String())
				continue
			}
			var recovery tracker.NativeRecovery
			decodeHubResponse(t, response, &recovery)
			if recovery.Issue.OrganizationID != org || recovery.Issue.WorkItemID != tracker.NativeWorkItemID(id) || recovery.ChangeDetail == nil || recovery.ChangeDetail.Change.OrganizationID != org || recovery.ChangeDetail.Change.ProjectID != "example" || recovery.ChangeDetail.Change.ID != fmt.Sprintf("change_%03d", index) || recovery.ChangeDetail.Summary.Status != "stale_policy" {
				t.Errorf("%s/%s: recovery lost scoped stale Change summary: %+v", org, id, recovery)
			}
		}

		secret := "example-preview-secret-" + string(org)
		tokenHash := apikey.HashToken(secret)
		expires := now.Add(time.Hour)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "preview_credential", Name: "Example runner", Hash: tokenHash, Fingerprint: tokenFingerprint(tokenHash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "preview_credential", ProjectID: "example"}); err != nil {
				return err
			}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: "preview_enrollment", RunnerID: "preview_runner", MachineID: "preview_machine", TokenHash: apikey.HashToken("example-enrollment-" + string(org)), Scope: "projects", ProjectIDs: []tracker.ProjectID{"example"}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), "preview_enrollment", "preview_runner", "preview_machine", now); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: "preview_machine", Hostname: "example-host", Capacity: 2, Version: "example"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: "preview_runner", MachineID: "preview_machine", TokenID: "preview_credential", EnrollmentID: "preview_enrollment", Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{"example"}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
				return err
			}
			for index := range 2 {
				id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%03d", index))
				item, err := w.WorkItemByNativeID(t.Context(), id)
				if err != nil {
					return err
				}
				state := "Todo"
				if index == 0 {
					state = "Merging"
				}
				item.WorkflowStateName = &state
				item, err = w.UpdateWorkItem(t.Context(), item)
				if err != nil {
					return err
				}
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "example", WorkItemID: id, Number: index + 1, Revision: tracker.Revision(item.Revision)}, Title: "Example", Body: workItemRecordBody, State: state, Actor: tracker.Actor{Kind: "human", PrincipalID: "test_hub_admin"}, CreatedAt: now, UpdatedAt: now}
				raw, err := json.Marshal(issue)
				if err != nil {
					return err
				}
				version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "example", WorkItemID: id, RecordID: string(id), Revision: item.Revision}, raw)
				if err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
					return err
				}
				if err := w.PutWorkItemFacts(t.Context(), claimstore.WorkItemFacts{OrganizationID: org, ProjectID: "example", WorkItemID: id}); err != nil {
					return err
				}
				if err := w.PutQueueEntry(t.Context(), claimstore.QueueEntry{OrganizationID: org, ProjectID: "example", WorkItemID: id, Scope: "example", State: state, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		base := "/api/v2/organizations/" + string(org) + "/projects/example"
		for _, session := range []string{"", "example-session"} {
			request := tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{PolicyID: nextDescriptor.ID, MachineID: "preview_machine", SessionID: session}, Limit: 2}
			response := performHubAPIRequest(t, service, http.MethodPost, base+"/claims/preview", secret, request)
			requireNativeStatus(t, response, http.StatusOK)
			var page tracker.NativeCapacityPage
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || page.Items[0].WorkItemID != "wi_001" || page.Items[0].OrganizationID != org || page.Items[0].ProjectID != "example" {
				t.Fatalf("stale-policy preview lost real scoped Todo candidate or admitted stale Merging: %+v", page.Items)
			}
			request.PolicyID = descriptor.ID
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base+"/claims/preview", secret, request), http.StatusConflict)
		}

		currentReview, err := content.PrepareReviewPolicy(t.Context(), eventstore.ReviewPolicy{OrganizationID: org, ProjectID: "example", PolicyRevisionID: nextDescriptor.ID, Revision: 2, UpdatedBy: "test_hub_admin", UpdatedAt: now}, defaultChangeReviewPolicy(nextDescriptor))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetReviewPolicy(t.Context(), currentReview, review.ReviewPolicyID)
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := readCurrentChangePolicyRows(t.Context(), r, scope)
			return err
		}); err != nil {
			t.Fatalf("current review policy rejected: %v", err)
		}
		currentResponse := performHubAPIRequest(t, service, http.MethodPost, base+"/claims/preview", secret, tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{PolicyID: nextDescriptor.ID, MachineID: "preview_machine"}, Limit: 2})
		requireNativeStatus(t, currentResponse, http.StatusOK)
		var currentPage tracker.NativeCapacityPage
		decodeHubResponse(t, currentResponse, &currentPage)
		if len(currentPage.Items) != 1 || currentPage.Items[0].WorkItemID != "wi_001" || currentPage.Items[0].OrganizationID != org {
			t.Fatalf("current-policy preview lost scoped candidate: %+v", currentPage.Items)
		}
		if org == "org_second" {
			foreign := performHubAPIRequest(t, service, http.MethodPost, base+"/claims/preview", "example-preview-secret-org_first", tracker.NativeCapacityPreview{NativeClaim: tracker.NativeClaim{PolicyID: nextDescriptor.ID, MachineID: "preview_machine"}})
			if foreign.Code != http.StatusUnauthorized && foreign.Code != http.StatusForbidden && foreign.Code != http.StatusNotFound {
				t.Fatalf("foreign organization preview status=%d", foreign.Code)
			}
		}

	}
}
