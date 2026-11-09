package hubserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCoreChangeImportPreservesChangesVersionsAndEvidence(t *testing.T) {
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
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native"}, Settings: projectstore.Settings{OrganizationID: organization, ProjectID: project}, States: nativeFixtureStates()}); err != nil {
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
			descriptor := hubTestPolicy()
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
			if err := postgres.ReadNeutralImportBundle(t.Context(), source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
				if err := validateCoreNeutralChangeImport(bundle); err != nil {
					return err
				}
				tx := newCoreNeutralImport(bundle)
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
				parity, err := ImportCoreReportLandings(t.Context(), repository, source)
				if err != nil || parity.Landings != 2 || parity.SourceSHA256 == "" || parity.ProjectionSHA256 == "" {
					t.Fatalf("source landing parity = %+v error %v", parity, err)
				}
				if landingDigest != "" && landingDigest != parity.ProjectionSHA256 {
					t.Fatal("source landing projection changed across replay")
				}
				landingDigest = parity.ProjectionSHA256
			}
			for range 2 {
				parity, err := ImportCoreReportEscapes(t.Context(), repository, source)
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
					if err != nil || string(bundle) != string(coreChangeBundle(version.BaseSHA, version.HeadSHA)) {
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
	_, err = db.ExecContext(t.Context(), `INSERT INTO native_attempts (organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,1,'succeeded',1,1,'example/payload',$7,$7)`, organization, id, issueID, leaseID, fencingToken, runID, at)
	return err
}

func coreChangeSourceCopy(t *testing.T, organization tracker.OrganizationID, project tracker.ProjectID, createdAt time.Time, descriptor policy.Descriptor, policyJSON []byte) string {
	t.Helper()
	return coreChangeNeutralFixture(t, organization, project, createdAt, descriptor, policyJSON).write()
}
func coreChangeNeutralFixture(t *testing.T, organization tracker.OrganizationID, project tracker.ProjectID, createdAt time.Time, descriptor policy.Descriptor, policyJSON []byte) *coreNeutralFixture {
	t.Helper()
	fixture := newCoreNeutralFixture(t, organization, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
	fixture.add("policy_revisions", map[string]any{"scope": string(organization) + "/" + string(project), "policy_id": descriptor.ID, "metadata_json": string(policyJSON)})
	for index, suffix := range []string{"a", "b"} {
		id := "change_import_" + suffix
		item := "wi_change_import_" + suffix
		change := tracker.ChangeRequest{ID: id, OrganizationID: organization, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(item), LinkedIssues: []tracker.NativeWorkItemID{tracker.NativeWorkItemID(item)}, Title: "Change " + id, Body: "Preserved discussion", CurrentVersion: "version_" + suffix, Revision: 2, CreatedAt: createdAt, UpdatedAt: createdAt.Add(time.Minute)}
		changeJSON, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		fixture.add("change_requests", map[string]any{"id": id, "organization_id": organization, "project_id": project, "work_item_id": item, "record_json": string(changeJSON)})
		fixture.add("change_issue_links", map[string]any{"change_id": id, "organization_id": organization, "project_id": project, "work_item_id": item})
		validation := &gate.CommandResult{HeadSHA: strings.Repeat("b", 40), ExitCode: 0, DurationNS: int64(time.Second), Command: "go test", TreeSHA: strings.Repeat("c", 40), Output: "validated output"}
		baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
		bundle := coreChangeBundle(baseSHA, headSHA)
		source := &tracker.ChangeSource{Format: "git-bundle", BaseSHA: baseSHA, HeadSHA: headSHA, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("e", 64), Bytes: int64(len(bundle))}
		externalID := fmt.Sprintf("%d", 201+index)
		version := tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: baseSHA, HeadSHA: headSHA, MergeBaseSHA: baseSHA, Repository: "https://github.com/example-org/example-project", AttemptID: "source_" + suffix, PolicyID: descriptor.ID, Code: tracker.ChangeArtifact{Kind: "code", URI: "bundle://example"}, Validation: validation, External: &tracker.ChangeExternalReference{Provider: "github", ID: externalID, URL: "https://github.com/example-org/example-project/pull/" + externalID}, Source: source}, ID: "version_" + suffix, ChangeID: id, Number: int64(index + 1), Policy: descriptor, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: createdAt}
		versionJSON, err := json.Marshal(version)
		if err != nil {
			t.Fatal(err)
		}
		fixture.add("change_versions", map[string]any{"id": version.ID, "change_id": id, "number": version.Number, "record_json": string(versionJSON), "policy_revision_id": descriptor.ID, "payload_object_key": "", "repository": version.Repository, "created_at": createdAt.Format(time.RFC3339Nano)})
		fixture.add("change_sources", map[string]any{"version_id": version.ID, "bundle": bundle, "object_key": "", "object_sha256": "", "object_bytes": 0, "content_bytes": 0})
		fixture.add("change_viewed_files", map[string]any{"version_id": version.ID, "principal_id": "example-owner", "manifest_sha256": strings.Repeat("d", 64), "file_sha256": strings.Repeat("f", 64), "viewed": 1})
		fixture.add("native_pr_publications", map[string]any{"version_id": version.ID, "change_id": id, "repository": version.Repository, "base_ref": "develop", "branch": "change/" + version.ID, "head_sha": headSHA, "policy_id": descriptor.ID, "source_attempt_id": version.AttemptID, "recorder_attempt_id": "recorder_" + version.ID, "provider": "github", "external_id": externalID, "external_url": version.Repository + "/pull/" + externalID})
		landing := tracker.NativeLandingReceipt{VersionID: version.ID, HeadSHA: headSHA, MergeSHA: strings.Repeat("c", 40), BaseRef: "develop", BaseSHA: baseSHA, Method: "merge", Landed: true, ObservedAt: createdAt.Add(time.Minute + 123*time.Nanosecond)}
		landingJSON, err := json.Marshal(landing)
		if err != nil {
			t.Fatal(err)
		}
		fixture.add("change_landing_receipts", map[string]any{"attempt_id": version.AttemptID, "version_id": version.ID, "record_json": string(landingJSON)})
		fixture.add("release_deliveries", map[string]any{"work_item_id": item, "organization_id": organization, "project_id": project, "source_issue_id": item, "source_change_id": id, "source_merge_sha": strings.Repeat("c", 40), "repository_id": 47, "repository": version.Repository, "pr_number": 300 + index + 1, "head_ref": "release-pick/" + version.ID, "head_sha": headSHA, "base_ref": "release/v1.0", "base_sha": strings.Repeat("d", 40), "frozen_sha": strings.Repeat("e", 40), "change_id": id, "version_id": version.ID, "principal_id": "example-owner", "actor_kind": "human", "via_key": "example-release-key", "associated_at": createdAt.Add(2*time.Minute + 123*time.Nanosecond).Format(time.RFC3339Nano)})
		discussion := tracker.ChangeDiscussion{ID: "discussion_" + suffix, VersionID: version.ID, Body: "Preserved discussion", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: createdAt.Add(2 * time.Minute), Escape: &tracker.QualityEscape{OccurrenceID: "occurrence_" + suffix, Kind: "scheduled_failure", Cause: "validator_miss", ObservedAt: createdAt.Add(90 * time.Second), HumanOverride: true}}
		discussionJSON, err := json.Marshal(discussion)
		if err != nil {
			t.Fatal(err)
		}
		fixture.add("change_evidence", map[string]any{"sequence": 1, "change_id": id, "version_id": version.ID, "kind": "discussion", "source_key": nil, "record_json": string(discussionJSON), "payload_object_key": ""})
	}
	return fixture
}

func coreChangeString(value string) *string { return &value }

func coreChangeBundle(base, head string) []byte {
	return []byte(fmt.Sprintf("# v2 git bundle\n%s refs/heads/main\n-%s base\n\nbundle payload", head, base))
}
