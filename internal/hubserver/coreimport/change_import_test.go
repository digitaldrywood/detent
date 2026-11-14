package coreimport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport/coreimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoreChangeImportPreservesChangesVersionsAndEvidence(t *testing.T) {
	t.Parallel()
	var native *pgxpool.Config
	repository, admin := coreImportPostgresWithAdmin(t, 3, func(config *pgxpool.Config) { native = config.Copy() })
	closeAfterTenants := t.Cleanup
	objects := &memoryRunDetails{}
	for _, organization := range []tracker.OrganizationID{"org_change_import", "org_change_import_other"} {
		t.Run(string(organization), func(t *testing.T) {
			project := tracker.ProjectID("prj_change_import")
			createdAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: "example-organization", CreatedAt: createdAt}); err != nil {
					return err
				}
				if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_change_import"}); err != nil {
					return err
				}
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native"}, Settings: projectstore.Settings{OrganizationID: organization, ProjectID: project}, States: hubtest.States()}); err != nil {
					return err
				}
				for index, id := range []tracker.NativeWorkItemID{"wi_change_import_a", "wi_change_import_b"} {
					_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ID: int64(70 + index), NativeID: id, ProjectID: project, Number: int64(index + 1), Revision: 1, EventSequence: 1, Title: "Example issue", ActorKind: coreChangeString("human"), ActorPrincipalID: coreChangeString("example-owner"), CreatedAt: createdAt, UpdatedAt: createdAt, NativeCreatedAt: createdAt, NativeUpdatedAt: createdAt, LastActivityAt: createdAt, WorkflowStateName: coreChangeString("Todo")})
					if err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			descriptor := hubtest.PolicyDescriptor()
			policyJSON, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			policyHash := strings.Repeat("a", 64)
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO policy_revisions (organization_id,project_id,policy_id,approved_by,approved_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) VALUES ($1,$2,$3,'owner_token_change_import',$4,$5,$6,1,1)`, organization, project, descriptor.ID, createdAt, "orgs/"+string(organization)+"/blobs/sha256/aa/"+policyHash, policyHash); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO machines (organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES ($1,'machine_change_import','example-host',8,'example-v1',$2,$2,$2)`, organization, createdAt); err != nil {
				t.Fatal(err)
			}
			for index, suffix := range []string{"a", "b"} {
				for _, attemptID := range []string{"source_" + suffix, "recorder_version_" + suffix} {
					if err := seedCoreChangeAttempt(t, admin, organization, int64(70+index), attemptID, createdAt); err != nil {
						t.Fatal(err)
					}
				}
			}
			copyPath := coreChangeSourceCopy(t, organization, project, createdAt, descriptor, policyJSON)
			source := CoreChangeImportSource{BundlePath: copyPath, OrganizationID: organization, SourceObjects: objects, Objects: objects}
			content := eventstore.NewContent(objects)
			if err := neutralimport.Read(t.Context(), source.BundlePath, source.OrganizationID, func(bundle *neutralimport.Bundle) error {
				if err := validateCoreNeutralChangeImport(bundle); err != nil {
					return err
				}
				tx := neutralimport.NewIndex(bundle)
				copyHash := bundle.Identity().SourceSHA256
				if err := repository.PinVerifiedNeutralImport(t.Context(), bundle, objects); err != nil {
					return err
				}
				page, err := readCoreChangePage(t.Context(), tx, source, "")
				if err != nil || len(page) == 0 {
					return fmt.Errorf("read initial change page: %w", err)
				}
				row := page[0]
				if err := importCoreChange(t.Context(), tx, repository, content, source, copyHash, row); err != nil {
					return err
				}
				if err := importCoreChangeVersions(t.Context(), tx, repository, content, source, copyHash, row.change.ID); err != nil {
					return err
				}
				if err := importCoreChangePublications(t.Context(), tx, repository, content, source, copyHash, row.change.ID); err != nil {
					return err
				}
				if err := importCoreChangeEvidence(t.Context(), tx, repository, content, source, copyHash, row.change.ID); err != nil {
					return err
				}
				if err := importCoreChangeLandings(t.Context(), tx, repository, content, source, copyHash, row.change.ID); err != nil {
					return err
				}
				return importCoreChangeViewedFiles(t.Context(), tx, repository, content, source, copyHash, row.change.ID)
			}); err != nil {
				t.Fatalf("prepare partial change import: %v", err)
			}
			if err := repository.ReadRecords(t.Context(), organization, corestore.Background, func(r eventstore.Reader) error {
				row, err := r.Change(t.Context(), "change_import_a")
				if err != nil {
					return err
				}
				if row.CurrentVersionID != nil {
					return fmt.Errorf("partial change import finalized its current version: %q", *row.CurrentVersionID)
				}
				return nil
			}); err != nil {
				t.Fatalf("assert partial change import state: %v", err)
			}
			for range 2 {
				count, err := ImportCoreChanges(t.Context(), repository, source)
				if err != nil || count != 2 {
					t.Fatalf("change import count=%d error=%v", count, err)
				}
			}
			var escapeDigest string
			if _, err := admin.ExecContext(t.Context(), `UPDATE change_requests SET landed_version_id=NULL,landed_at=NULL,landed_head_sha=NULL,landed_merge_sha=NULL,landed_base_ref=NULL WHERE organization_id=$1`, organization); err != nil {
				t.Fatal(err)
			}
			var landingDigest string
			for range 2 {
				parity, err := importCoreReportLandings(t.Context(), repository, source)
				if err != nil || parity.Landings != 2 || parity.SourceSHA256 == "" || parity.ProjectionSHA256 == "" {
					t.Fatalf("source landing parity = %+v error %v", parity, err)
				}
				if landingDigest != "" && landingDigest != parity.ProjectionSHA256 {
					t.Fatal("source landing projection changed across replay")
				}
				landingDigest = parity.ProjectionSHA256
			}
			for range 2 {
				parity, err := importCoreReportEscapes(t.Context(), repository, source)
				if err != nil || parity.Escapes != 2 || parity.SourceSHA256 == "" || parity.ProjectionSHA256 == "" {
					t.Fatalf("source escape parity = %+v error %v", parity, err)
				}
				if escapeDigest != "" && escapeDigest != parity.ProjectionSHA256 {
					t.Fatal("escape source projection changed across replay")
				}
				escapeDigest = parity.ProjectionSHA256
			}
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}
			pool, err := pgxpool.NewWithConfig(t.Context(), native.Copy())
			if err != nil {
				t.Fatal(err)
			}
			repository, err = postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
			if err != nil {
				pool.Close()
				t.Fatal(err)
			}
			reopened := repository
			closeAfterTenants(func() {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := repository.ReadRecords(t.Context(), organization, corestore.Serving, func(r eventstore.Reader) error {
				for _, id := range []string{"change_import_a", "change_import_b"} {
					changeRow, err := r.Change(t.Context(), id)
					if err != nil {
						return err
					}
					if changeRow.LandedAt == nil || changeRow.LandedVersionID == nil || changeRow.LandedMergeSHA == nil || *changeRow.LandedMergeSHA != strings.Repeat("c", 40) {
						return errors.New("legacy landing receipt did not populate typed report landing facts")
					}
					wantVersion := map[string]string{"change_import_a": "version_a", "change_import_b": "version_b"}[id]
					if changeRow.CurrentVersionID == nil || *changeRow.CurrentVersionID != wantVersion {
						return fmt.Errorf("imported current version=%v want=%s", changeRow.CurrentVersionID, wantVersion)
					}
					head, err := r.Version(t.Context(), id, *changeRow.CurrentVersionID)
					if err != nil || head.ID != wantVersion || head.ChangeID != id || head.HeadSHA != strings.Repeat("b", 40) {
						return fmt.Errorf("imported current head=%+v error=%v", head, err)
					}
					change, err := content.Change(t.Context(), changeRow)
					if err != nil || change.Title != "Change "+id || change.Body != "Preserved discussion" || change.LinkedIssues[0] == "" {
						return fmt.Errorf("change %s was not preserved: %+v: %w", id, change, err)
					}
					versions, err := r.Versions(t.Context(), id, 0, 10)
					if err != nil || len(versions) != 1 {
						return fmt.Errorf("change %s versions=%d: %w", id, len(versions), err)
					}
					version, err := content.Version(t.Context(), versions[0])
					if err != nil || version.HeadSHA != strings.Repeat("b", 40) || version.Policy.ID != descriptor.ID || version.Validation == nil || version.Validation.Output != "validated output" {
						return fmt.Errorf("change %s version was not hydrated: %+v: %w", id, version, err)
					}
					publication, err := repository.CoreNativePRPublication(t.Context(), organization, version.ID)
					if err != nil || publication.ChangeID != id || publication.VersionID != version.ID || publication.Repository != version.Repository || publication.BaseRef != "develop" || publication.Branch != "change/"+version.ID || publication.HeadSHA != version.HeadSHA || publication.PolicyID != descriptor.ID || publication.SourceAttemptID != version.AttemptID || publication.RecorderAttemptID != "recorder_"+version.ID || publication.External.Provider != "github" || publication.External.ID != fmt.Sprintf("%d", 200+version.Number) || publication.External.URL != publication.Repository+"/pull/"+fmt.Sprintf("%d", 200+version.Number) {
						return fmt.Errorf("change %s publication was not preserved: %+v: %w", id, publication, err)
					}
					var landing eventstore.LandingReceipt
					if err := repository.ReadRecords(t.Context(), organization, corestore.Background, func(r eventstore.Reader) error {
						var err error
						landing, err = r.LandingReceipt(t.Context(), "source_"+version.ID[len(version.ID)-1:])
						return err
					}); err != nil {
						return err
					}
					if landing.VersionID != version.ID || landing.MergeSHA != strings.Repeat("c", 40) {
						return fmt.Errorf("change %s landing receipt was not preserved: %+v", id, landing)
					}
					landingPayload, err := content.LandingReceipt(t.Context(), landing)
					if err != nil || !landingPayload.ObservedAt.Equal(createdAt.Add(time.Minute+123*time.Nanosecond)) {
						return fmt.Errorf("change %s landing timestamp was not preserved: %+v: %w", id, landingPayload, err)
					}
					bundle, err := content.SourceBundle(t.Context(), organization, versions[0], *version.Source)
					if err != nil || string(bundle) != string(coreimporttest.ChangeBundle(version.BaseSHA, version.HeadSHA)) {
						return fmt.Errorf("change %s source bundle was not preserved: %w", id, err)
					}
					viewed, err := r.ViewedFiles(t.Context(), version.ID, "example-owner", 10)
					if err != nil || len(viewed) != 1 || !viewed[0].Viewed || viewed[0].ManifestSHA256 != strings.Repeat("d", 64) {
						return fmt.Errorf("change %s viewed file was not preserved: %+v: %w", id, viewed, err)
					}
					evidence, err := r.Evidence(t.Context(), id, 0, 10)
					if err != nil || len(evidence) != 1 || evidence[0].Sequence != 1 || evidence[0].Kind != "discussion" {
						return fmt.Errorf("change %s evidence=%+v: %w", id, evidence, err)
					}
					payload, err := content.Evidence(t.Context(), evidence[0])
					var discussion tracker.ChangeDiscussion
					if err != nil || json.Unmarshal(payload, &discussion) != nil || discussion.Body != "Preserved discussion" {
						return fmt.Errorf("change %s discussion was not preserved: %w", id, err)
					}
					delivery, err := repository.CoreReleaseDelivery(t.Context(), organization, project, tracker.NativeWorkItemID("wi_"+id))
					if err != nil || delivery.OrganizationID != organization || delivery.ProjectID != project || delivery.WorkItemID != tracker.NativeWorkItemID("wi_"+id) || delivery.SourceIssueID != tracker.NativeWorkItemID("wi_"+id) || delivery.SourceChangeID != id || delivery.SourceMergeSHA != strings.Repeat("c", 40) || delivery.RepositoryID == nil || *delivery.RepositoryID != 47 || delivery.Repository != "https://github.com/example-org/example-project" || delivery.PRNumber != 300+int(versions[0].Number) || delivery.VersionID != version.ID || delivery.Actor.PrincipalID != "example-owner" || delivery.Actor.Kind != "human" || delivery.Actor.ViaKey != "example-release-key" || !delivery.AssociatedAt.Equal(createdAt.Add(2*time.Minute)) {
						return fmt.Errorf("change %s release delivery was not preserved: %+v: %w", id, delivery, err)
					}
					var releaseReceipt eventstore.Receipt
					if err := repository.ReadRecords(t.Context(), organization, corestore.Background, func(r eventstore.Reader) error {
						var err error
						releaseReceipt, err = r.Receipt(t.Context(), "production-import", "core-release-delivery", string(delivery.WorkItemID))
						return err
					}); err != nil {
						return err
					}
					releasePayload, err := content.Receipt(t.Context(), releaseReceipt)
					var releaseProof struct {
						Value eventstore.ReleaseDelivery `json:"value"`
					}
					if err != nil || json.Unmarshal(releasePayload, &releaseProof) != nil || !releaseProof.Value.AssociatedAt.Equal(createdAt.Add(2*time.Minute+123*time.Nanosecond)) {
						return fmt.Errorf("change %s exact release timestamp was not retained in its import receipt: %w", id, err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var frozenRows []eventstore.VersionLanding
			if err := repository.ReadRecords(t.Context(), organization, corestore.Serving, func(r eventstore.Reader) error {
				owner, ok := r.(eventstore.VersionLandingReader)
				if !ok {
					return errors.New("version landing owner unavailable")
				}
				for _, suffix := range []string{"a", "b"} {
					version, err := r.Version(t.Context(), "change_import_"+suffix, "version_"+suffix)
					if err != nil {
						return err
					}
					if !version.LandingHistoryKnown || version.FrozenLanding == nil {
						return errors.New("verified source version history remained unknown")
					}
					frozen, err := owner.VersionLanding(t.Context(), "change_import_"+suffix, "version_"+suffix)
					if err != nil {
						return err
					}
					frozenRows = append(frozenRows, frozen)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			objects.beforeRead = func(context.Context) error {
				if held := repository.Stats().AcquiredConns(); held != 0 {
					return fmt.Errorf("frozen import object read retained %d database connections", held)
				}
				return nil
			}
			for index, frozen := range frozenRows {
				value, err := content.VersionLanding(t.Context(), frozen)
				expected := []string{"Frozen contract a", "Frozen contract b"}[index]
				if err != nil || value.Quality == nil || value.Quality.IssueBody != expected || len(value.Quality.Criteria) != 1 || value.Quality.Criteria[0] != "Preserved criterion" || !value.LandedAt.Equal(createdAt.Add(time.Minute)) {
					t.Fatalf("verified frozen source=%+v error=%v", value, err)
				}
			}
			objects.beforeRead = nil
		})
	}
}

func seedCoreChangeAttempt(t *testing.T, db *sql.DB, organization tracker.OrganizationID, issueID int64, id string, at time.Time) error {
	t.Helper()
	runID, leaseID, sessionID := "run_"+id, "lease_"+id, "session_"+id
	if _, err := db.ExecContext(t.Context(), `INSERT INTO native_attempt_runs (organization_id,run_id,issue_id) VALUES ($1,$2,$3)`, organization, runID, issueID); err != nil {
		return err
	}
	var fencingToken int64
	err := db.QueryRowContext(t.Context(), `INSERT INTO leases (organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at,release_reason)
VALUES ($1,$2,$3,'machine_change_import',$4,1,$5,$5,$6,$7,'completed') RETURNING fencing_token`, organization, leaseID, issueID, sessionID, at, at.Add(time.Hour), at.Add(time.Second)).Scan(&fencingToken)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO native_attempts (organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at,project_id)
VALUES ($1,$2,$3,$4,$5,$6,1,'succeeded',1,1,'example/payload',$7,$7,(SELECT project_id FROM work_item_identities WHERE organization_id=$1 AND id=$3))`, organization, id, issueID, leaseID, fencingToken, runID, at)
	return err
}

func coreChangeSourceCopy(t *testing.T, organization tracker.OrganizationID, project tracker.ProjectID, createdAt time.Time, descriptor policy.Descriptor, policyJSON []byte) string {
	t.Helper()
	return coreimporttest.Changes(t, repositoryFS, organization, project, createdAt, descriptor, policyJSON).Write()
}

func coreChangeString(value string) *string { return &value }

type pausedDetails struct {
	attachment.DetailStore
	marker  []byte
	failure error
	fired   bool
}

func (s *pausedDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if !s.fired && bytes.Contains(raw, s.marker) {
		s.fired = true
		return attachment.ArchiveReference{}, s.failure
	}
	return s.DetailStore.StoreDetail(ctx, organization, raw)
}

func TestCoreChangeImportInterruptedLandingLeavesUnprovenState(t *testing.T) {
	t.Parallel()
	repository := coreImportPostgres(t)
	objects := &memoryRunDetails{}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: "org_unproven_import", Name: "Example organization", Slug: "example-unproven", Now: at})
	project := hubtest.Project(t, repository, organization.OrganizationID, hubtest.ProjectOptions{Name: "Unproven project"})
	item := hubtest.Issue(t, repository, objects, project, hubtest.IssueOptions{Title: "Unproven historical item", Now: at}).Issue
	descriptor := hubtest.PolicyDescriptor()
	unprovenVersion := tracker.ChangeVersion{ID: "unproven_historical_version", ChangeID: "unproven_historical_change", Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), Repository: "owner/repository", PolicyID: descriptor.ID}, Policy: descriptor, Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, CreatedAt: at.Add(-time.Minute)}
	unprovenLanding := tracker.ChangeLanding{VersionID: unprovenVersion.ID, HeadSHA: unprovenVersion.HeadSHA, MergeSHA: strings.Repeat("4", 40), BaseRef: "develop", LandedAt: at, Actor: unprovenVersion.Actor}
	unprovenChange := tracker.ChangeRequest{ID: unprovenVersion.ChangeID, OrganizationID: project.OrganizationID, ProjectID: project.ID, WorkItemID: item.WorkItemID, LinkedIssues: []tracker.NativeWorkItemID{item.WorkItemID}, Revision: 1, Title: "Unproven imported landing", CurrentVersion: unprovenVersion.ID, Landed: &unprovenLanding, CreatedAt: at.Add(-time.Hour), UpdatedAt: at}
	encode := func(value any) string {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	unprovenSource := newCoreNeutralFixture(t, project.OrganizationID, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "quality_landings", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
	unprovenSource.Add("change_requests", map[string]any{"id": unprovenChange.ID, "organization_id": project.OrganizationID, "project_id": project.ID, "work_item_id": item.WorkItemID, "created_at": nil, "record_json": encode(unprovenChange)})
	unprovenSource.Add("change_issue_links", map[string]any{"change_id": unprovenChange.ID, "organization_id": project.OrganizationID, "project_id": project.ID, "work_item_id": item.WorkItemID})
	unprovenSource.Add("change_versions", map[string]any{"id": unprovenVersion.ID, "change_id": unprovenChange.ID, "number": 1, "policy_revision_id": descriptor.ID, "repository": "owner/repository", "record_json": encode(unprovenVersion)})
	unprovenSource.Add("policy_revisions", map[string]any{"scope": string(project.OrganizationID) + "/" + string(project.ID), "policy_id": descriptor.ID, "metadata_json": encode(descriptor)})
	freezePause := errors.New("historical landing freeze pending")
	paused := &pausedDetails{DetailStore: objects, marker: []byte(`"merge_sha":"` + strings.Repeat("4", 40) + `"`), failure: freezePause}
	unprovenImport := CoreChangeImportSource{BundlePath: unprovenSource.Write(), OrganizationID: project.OrganizationID, SourceObjects: objects, Objects: paused}
	if err := neutralimport.Read(t.Context(), unprovenImport.BundlePath, project.OrganizationID, func(bundle *neutralimport.Bundle) error {
		if err := validateCoreNeutralChangeImport(bundle); err != nil {
			return err
		}
		if err := repository.PinVerifiedNeutralImport(t.Context(), bundle, objects); err != nil {
			return err
		}
		tx := neutralimport.NewIndex(bundle)
		rows, err := readCoreChangePage(t.Context(), tx, unprovenImport, "")
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return eventstore.ErrInvalidRecord
		}
		if err := importCoreChange(t.Context(), tx, repository, eventstore.NewContent(objects), unprovenImport, bundle.Identity().SourceSHA256, rows[0]); err != nil {
			return err
		}
		return importCoreChangeVersions(t.Context(), tx, repository, eventstore.NewContent(paused), unprovenImport, bundle.Identity().SourceSHA256, unprovenChange.ID)
	}); !errors.Is(err, freezePause) {
		t.Fatalf("interrupted landing freeze=%v", err)
	}
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		change, err := r.Change(t.Context(), unprovenChange.ID)
		if err != nil {
			return err
		}
		version, err := r.Version(t.Context(), unprovenChange.ID, unprovenVersion.ID)
		if err != nil {
			return err
		}
		if change.LandedVersionID == nil || *change.LandedVersionID != unprovenVersion.ID || version.LandingHistoryKnown || version.FrozenLanding != nil {
			t.Fatalf("fixture lost actual unproven import state: change=%+v version=%+v", change, version)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
