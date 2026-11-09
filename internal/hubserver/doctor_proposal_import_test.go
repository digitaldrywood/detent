package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDoctorProposalNeutralImportRepairsOnlyUnchangedHead(t *testing.T) {
	var native *pgxpool.Config
	repository, admin := coreImportPostgresWithAdmin(t, 3, func(config *pgxpool.Config) { native = config.Copy() })
	objects := &memoryRunDetails{}
	content := eventstore.NewContent(objects)
	marker := "<!-- detent:self-improvement proposal_id=example-imported -->"
	body := strings.Repeat("x", 70000) + "\n" + marker + "\n```detent-origin\norigin_kind: doctor\nfingerprint: example-imported-fingerprint\n```"
	orgs := []tracker.OrganizationID{"org_import_first", "org_import_second"}
	lookup := func(org tracker.OrganizationID) (postgres.DoctorProposalIdentityResult, error) {
		var result postgres.DoctorProposalIdentityResult
		err := repository.ReadDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(r postgres.DoctorProposalIdentityReader) error {
			var err error
			result, err = r.LookupDoctorProposalIdentity(t.Context(), postgres.DoctorProposalIdentitySelection{ProjectID: tracker.ProjectID("prj_" + string(org)), Identity: postgres.DoctorProposalIdentity{Kind: "marker", Value: marker}})
			return err
		})
		return result, err
	}
	for _, org := range orgs {
		project := tracker.ProjectID("prj_" + string(org))
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		source := CoreWorkItemImportSource{BundlePath: coreWorkItemNeutralFixtureBody(t, org, project, false, body).write(), OrganizationID: org, ProjectID: project, Objects: objects}
		if count, err := ImportCoreWorkItems(t.Context(), repository, source); err != nil || count != 2 {
			t.Fatalf("initial import count=%d error=%v", count, err)
		}
		result, err := lookup(org)
		if err != nil || result.WorkItemID != "wi_core_import_second" || result.Number != 3 {
			t.Fatalf("full imported marker lookup = %+v error=%v", result, err)
		}
		for _, statement := range []string{`UPDATE issues SET doctor_identity_revision=NULL WHERE organization_id=$1`, `DELETE FROM doctor_proposal_identities WHERE organization_id=$1`} {
			if _, err := admin.ExecContext(t.Context(), statement, org); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := lookup(org); !errors.Is(err, postgres.ErrDoctorProposalIdentitiesPending) {
			t.Fatalf("legacy metadata was treated as empty: %v", err)
		}
		liveBody := "Current live body"
		liveIdentities := postgres.DeriveDoctorProposalIdentities(liveBody)
		bodyRaw, err := json.Marshal(liveBody)
		if err != nil {
			t.Fatal(err)
		}
		bodyRef, err := objects.StoreDetail(t.Context(), string(org), bodyRaw)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: "wi_core_import", Number: 1, Revision: 3, Profile: "native"}, Title: "Live title", Body: liveBody, State: "Todo"}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: snapshot.WorkItemID, RecordID: string(snapshot.WorkItemID), Revision: 3}, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			item, err := w.WorkItem(t.Context(), project, snapshot.WorkItemID)
			if err != nil {
				return err
			}
			item.Title, item.BodyObjectKey = snapshot.Title, new(bodyRef.Key)
			if _, err := w.UpdateWorkItem(t.Context(), item); err != nil {
				return err
			}
			_, err = w.CreateCollaborationVersion(t.Context(), version)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION reject_doctor_import_source_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$ DECLARE unchanged record; BEGIN IF TG_TABLE_NAME='issues' AND TG_OP='UPDATE' THEN unchanged:=NEW; unchanged.doctor_identity_revision:=OLD.doctor_identity_revision; IF unchanged IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF; END IF; RAISE EXCEPTION 'unexpected source rewrite during identity repair'; END $$`); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"issues", "collaboration_versions", "collaboration_events", "native_commands", "issue_claim_facts", "queue_entries", "native_comments"} {
			if _, err := admin.ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER doctor_import_source_unchanged BEFORE INSERT OR UPDATE OR DELETE ON %s FOR EACH ROW EXECUTE FUNCTION reject_doctor_import_source_rewrite()`, table)); err != nil {
				t.Fatal(err)
			}
		}
		var before []byte
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			item, err := r.WorkItem(t.Context(), project, "wi_core_import_second")
			if err != nil {
				return err
			}
			before, err = json.Marshal(item)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if count, err := ImportCoreWorkItems(t.Context(), repository, source); err != nil || count != 2 {
			t.Fatalf("receipt identity repair count=%d error=%v", count, err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			item, err := r.WorkItem(t.Context(), project, "wi_core_import")
			if err != nil {
				return err
			}
			if item.Revision != 3 || item.Title != "Live title" || item.DoctorIdentityRevision != nil || stringValue(item.BodyObjectKey) != bodyRef.Key {
				return errors.New("receipt replay changed or rebound advanced source")
			}
			unchanged, err := r.WorkItem(t.Context(), project, "wi_core_import_second")
			if err != nil {
				return err
			}
			after, err := json.Marshal(unchanged)
			if err != nil {
				return err
			}
			if unchanged.DoctorIdentityRevision == nil || *unchanged.DoctorIdentityRevision != 2 || !bytes.Equal(before, after) {
				return errors.New("unchanged imported head was not repaired independently of source payload")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"issues", "collaboration_versions", "collaboration_events", "native_commands", "issue_claim_facts", "queue_entries", "native_comments"} {
			if _, err := admin.ExecContext(t.Context(), fmt.Sprintf(`DROP TRIGGER doctor_import_source_unchanged ON %s`, table)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admin.ExecContext(t.Context(), `DROP FUNCTION reject_doctor_import_source_rewrite()`); err != nil {
			t.Fatal(err)
		}
		if _, err := lookup(org); !errors.Is(err, postgres.ErrDoctorProposalIdentitiesPending) {
			t.Fatalf("advanced unknown head was treated as empty: %v", err)
		}
		if err := repository.WriteDoctorProposalIdentities(t.Context(), org, corestore.Serving, func(w postgres.DoctorProposalIdentityWriter) error {
			return w.BindDoctorProposalIdentities(t.Context(), postgres.DoctorProposalIdentityBinding{OrganizationID: org, ProjectID: project, WorkItemID: "wi_core_import", Revision: 3, VersionSHA256: version.PayloadSHA256, Identities: liveIdentities})
		}); err != nil {
			t.Fatal(err)
		}
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
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, org := range orgs {
		result, err := lookup(org)
		if err != nil || result.WorkItemID != "wi_core_import_second" || result.Revision != 2 {
			t.Fatalf("reopened imported identities = %+v error=%v", result, err)
		}
	}
}
