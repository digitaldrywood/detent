package hubserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoreChangeImportPreservesChangesVersionsAndEvidence(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 2)
	objects := &memoryRunDetails{}
	organization := tracker.OrganizationID("org_change_import")
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
	source := CoreChangeImportSource{CopyPath: copyPath, OrganizationID: organization, SourceObjects: objects, Objects: objects}
	content := eventstore.NewContent(objects)
	if err := postgres.ReadCoreImportCopy(t.Context(), source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		if err := repository.PinCoreImportSource(t.Context(), organization, copyHash, objects); err != nil {
			return err
		}
		page, err := readCoreChangePage(t.Context(), tx, organization, "")
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
	if err := repository.ReadRecords(t.Context(), organization, corestore.Serving, func(r eventstore.Reader) error {
		for _, id := range []string{"change_import_a", "change_import_b"} {
			changeRow, err := r.Change(t.Context(), id)
			if err != nil {
				return err
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
	path := t.TempDir() + "/change-source.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE change_requests (id TEXT,organization_id TEXT,project_id TEXT,work_item_id TEXT,record_json TEXT)`,
		`CREATE TABLE change_issue_links (change_id TEXT,organization_id TEXT,project_id TEXT,work_item_id TEXT)`,
		`CREATE TABLE change_versions (id TEXT,change_id TEXT,number INTEGER,record_json TEXT,policy_revision_id TEXT,payload_object_key TEXT,repository TEXT,created_at TEXT)`,
		`CREATE TABLE change_evidence (sequence INTEGER,change_id TEXT,version_id TEXT,kind TEXT,source_key TEXT,record_json TEXT,payload_object_key TEXT)`,
		`CREATE TABLE change_sources (version_id TEXT,bundle BLOB,object_key TEXT,object_sha256 TEXT,object_bytes INTEGER,content_bytes INTEGER)`,
		`CREATE TABLE change_landing_receipts (attempt_id TEXT,version_id TEXT,record_json TEXT)`,
		`CREATE TABLE change_viewed_files (version_id TEXT,principal_id TEXT,manifest_sha256 TEXT,file_sha256 TEXT,viewed INTEGER)`,
		`CREATE TABLE policy_revisions (scope TEXT,policy_id TEXT,metadata_json TEXT)`,
		`CREATE TABLE change_command_outputs (version_id TEXT,review_id TEXT,role TEXT,position INTEGER,organization_id TEXT,object_key TEXT,object_sha256 TEXT,object_bytes INTEGER,content_bytes INTEGER)`,
		`CREATE TABLE change_finding_attributions (version_id TEXT,record_id TEXT,role TEXT,position INTEGER,attribution TEXT,base_sha TEXT,head_sha TEXT,command TEXT,diagnostic TEXT,head_output_digest TEXT,base_output_digest TEXT,base_exit_code INTEGER)`,
		`CREATE TABLE change_content_objects (organization_id TEXT,object_key TEXT,object_sha256 TEXT,object_bytes INTEGER,content_bytes INTEGER)`,
		`CREATE TABLE attachments (organization_id TEXT,object_key TEXT,sha256 TEXT,size INTEGER,uncompressed_size INTEGER,kind TEXT,customer_owned INTEGER,storage_binding_id TEXT)`,
		`CREATE TABLE native_pr_publications (version_id TEXT,change_id TEXT,repository TEXT,base_ref TEXT,branch TEXT,head_sha TEXT,policy_id TEXT,source_attempt_id TEXT,recorder_attempt_id TEXT,provider TEXT,external_id TEXT,external_url TEXT)`,
		`CREATE TABLE release_deliveries (work_item_id TEXT,organization_id TEXT,project_id TEXT,source_issue_id TEXT,source_change_id TEXT,source_merge_sha TEXT,repository_id INTEGER,repository TEXT,pr_number INTEGER,head_ref TEXT,head_sha TEXT,base_ref TEXT,base_sha TEXT,frozen_sha TEXT,change_id TEXT,version_id TEXT,principal_id TEXT,actor_kind TEXT,via_key TEXT,associated_at TEXT)`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO policy_revisions(scope,policy_id,metadata_json) VALUES(?,?,?)`, string(organization)+"/"+string(project), descriptor.ID, string(policyJSON)); err != nil {
		t.Fatal(err)
	}
	for index, suffix := range []string{"a", "b"} {
		id := "change_import_" + suffix
		item := "wi_change_import_" + suffix
		change := tracker.ChangeRequest{ID: id, OrganizationID: organization, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(item), LinkedIssues: []tracker.NativeWorkItemID{tracker.NativeWorkItemID(item)}, Title: "Change " + id, Body: "Preserved discussion", CurrentVersion: "version_" + suffix, Revision: 2, CreatedAt: createdAt, UpdatedAt: createdAt.Add(time.Minute)}
		changeJSON, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO change_requests VALUES(?,?,?,?,?)`, id, organization, project, item, string(changeJSON)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO change_issue_links VALUES(?,?,?,?)`, id, organization, project, item); err != nil {
			t.Fatal(err)
		}
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
		if _, err := db.ExecContext(t.Context(), `INSERT INTO change_versions(id,change_id,number,record_json,policy_revision_id,payload_object_key,repository,created_at) VALUES(?,?,?,?,?, '',?,?)`, version.ID, id, version.Number, string(versionJSON), descriptor.ID, version.Repository, createdAt.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO change_sources(version_id,bundle,object_key,object_sha256,object_bytes,content_bytes) VALUES(?,?,'','',0,0)`, version.ID, bundle); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO change_viewed_files(version_id,principal_id,manifest_sha256,file_sha256,viewed) VALUES(?,?,?,?,1)`, version.ID, "example-owner", strings.Repeat("d", 64), strings.Repeat("f", 64)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO native_pr_publications VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, version.ID, id, version.Repository, "develop", "change/"+version.ID, headSHA, descriptor.ID, version.AttemptID, "recorder_"+version.ID, "github", externalID, version.Repository+"/pull/"+externalID); err != nil {
			t.Fatal(err)
		}
		landing := tracker.NativeLandingReceipt{VersionID: version.ID, HeadSHA: headSHA, MergeSHA: strings.Repeat("c", 40), BaseRef: "develop", BaseSHA: baseSHA, Method: "merge", Landed: true, ObservedAt: createdAt.Add(time.Minute + 123*time.Nanosecond)}
		landingJSON, err := json.Marshal(landing)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO change_landing_receipts VALUES(?,?,?)`, version.AttemptID, version.ID, string(landingJSON)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO release_deliveries VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, item, organization, project, item, id, strings.Repeat("c", 40), 47, version.Repository, 300+index+1, "release-pick/"+version.ID, headSHA, "release/v1.0", strings.Repeat("d", 40), strings.Repeat("e", 40), id, version.ID, "example-owner", "human", "example-release-key", createdAt.Add(2*time.Minute+123*time.Nanosecond).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		discussion := tracker.ChangeDiscussion{ID: "discussion_" + suffix, VersionID: version.ID, Body: "Preserved discussion", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: createdAt.Add(time.Second)}
		discussionJSON, err := json.Marshal(discussion)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO change_evidence VALUES(?,?,?,?,?,?, '')`, 1, id, version.ID, "discussion", nil, string(discussionJSON)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

func coreChangeString(value string) *string { return &value }

func coreChangeBundle(base, head string) []byte {
	return []byte(fmt.Sprintf("# v2 git bundle\n%s refs/heads/main\n-%s base\n\nbundle payload", head, base))
}
