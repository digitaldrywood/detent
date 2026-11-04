package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

func TestPostgresProjectActionsUseTenantOwner(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC()
	organizations := []tracker.OrganizationID{"org_startup", "org_action_other"}
	for _, organization := range organizations {
		seedStartupOrganization(t, repository, organization)
		err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "action_operator_" + string(organization), Name: "Example operator", Hash: apikey.HashToken("action-token-" + string(organization)), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, KeyScope: "admin", ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: tracker.ProjectID("project_" + string(organization)), Name: "Example", Slug: "example", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()})
			if err != nil {
				return err
			}
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "action_operator_" + string(organization), ProjectID: tracker.ProjectID("project_" + string(organization))})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{echo: echo.New(), database: &database{postgres: repository}, secretRewrapReady: make(chan struct{}), config: Config{Logger: discardLogger(), TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }}}
	service.ready.Store(true)
	service.echo.Use(middleware.Recover())
	service.registerNativeRoutes(service.echo)
	for _, organization := range organizations {
		t.Run(string(organization), func(t *testing.T) {
			path := "/api/v2/organizations/" + string(organization) + "/projects/project_" + string(organization) + "/actions"

			call := func(method, suffix, body string, status int) []byte {
				t.Helper()
				request := httptest.NewRequest(method, path+suffix, strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer action-token-"+string(organization))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				service.echo.ServeHTTP(response, request)
				if response.Code != status {
					t.Fatalf("%s %s status=%d want %d: %s", method, suffix, response.Code, status, response.Body.String())
				}
				return response.Body.Bytes()
			}
			create := `{"idempotency_key":"create-action","name":"Example action","command":"printf example","keybinding":"mod+shift+k"}`
			call(http.MethodGet, "", "", http.StatusOK)
			created := call(http.MethodPost, "", create, http.StatusCreated)
			var action workspacesession.Action
			if err := json.Unmarshal(created, &action); err != nil {
				t.Fatal(err)
			}
			if action.ID == "" || action.Revision != 1 || action.Command != "printf example" || action.OrganizationID != string(organization) {
				t.Fatalf("unexpected action: %+v", action)
			}
			if replay := call(http.MethodPost, "", create, http.StatusCreated); string(replay) != string(created) {
				t.Fatalf("create replay differs: %s", replay)
			}
			call(http.MethodPost, "", strings.Replace(create, "printf example", "printf different", 1), http.StatusConflict)
			suffix := "/" + action.ID
			patch := `{"idempotency_key":"patch-action","expected_revision":1,"name":"Updated action","command":"printf updated"}`
			updated := call(http.MethodPatch, suffix, patch, http.StatusOK)
			var changed workspacesession.Action
			if err := json.Unmarshal(updated, &changed); err != nil {
				t.Fatal(err)
			}
			if changed.Revision != 2 || changed.Command != "printf updated" || changed.Name != "Updated action" || changed.Keybinding != action.Keybinding {
				t.Fatalf("unexpected update: %+v", changed)
			}

			assertCommandOwner := func(want int, command string) {
				t.Helper()
				var count int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM archive_object_owners WHERE organization_id=$1 AND owner_kind='project-action' AND owner_id=$2`, organization, actionstore.ArchiveOwnerID(tracker.ProjectID("project_"+string(organization)), action.ID)).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != want {
					t.Fatalf("command owners=%d want %d", count, want)
				}
				if want == 0 {
					return
				}
				err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					record, err := r.(actionstore.Reader).ProjectAction(t.Context(), tracker.ProjectID("project_"+string(organization)), action.ID)
					if err != nil {
						return err
					}
					if record.Action.Command != "" {
						t.Fatal("command bytes stored in typed metadata")
					}
					object, err := r.ArchiveObjectByReference(t.Context(), record.Command)
					if err != nil {
						return err
					}
					var owned bool
					if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM archive_object_owners WHERE organization_id=$1 AND owner_kind='project-action' AND owner_id=$2 AND archive_id=$3)`, organization, actionstore.ArchiveOwnerID(tracker.ProjectID("project_"+string(organization)), action.ID), object.ID).Scan(&owned); err != nil {
						return err
					}
					if !owned {
						t.Fatal("current command has no archive owner")
					}
					raw, err := service.runDetailStore().ReadDetail(t.Context(), string(organization), record.Command)
					if err != nil {
						return err
					}
					if string(raw) != command {
						t.Fatalf("archived command=%q want %q", raw, command)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			assertCommandOwner(1, "printf updated")
			if replay := call(http.MethodPatch, suffix, patch, http.StatusOK); string(replay) != string(updated) {
				t.Fatalf("patch replay differs: %s", replay)
			}
			call(http.MethodPatch, suffix, strings.Replace(patch, "patch-action", "stale-action", 1), http.StatusConflict)
			var listed projectActionList
			if err := json.Unmarshal(call(http.MethodGet, "", "", http.StatusOK), &listed); err != nil {
				t.Fatal(err)
			}
			if len(listed.Items) != 1 || listed.Items[0].Command != "printf updated" {
				t.Fatalf("unexpected listing: %+v", listed)
			}
			other := organizations[0]
			if other == organization {
				other = organizations[1]
			}
			if err := repository.ReadTenant(t.Context(), other, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.(actionstore.Reader).ProjectAction(t.Context(), tracker.ProjectID("project_"+string(organization)), action.ID)
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("cross-tenant action read: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			call(http.MethodPost, suffix+"/runs", `{"idempotency_key":"run-action","workspace_id":"workspace_example"}`, http.StatusServiceUnavailable)
			credentialID := "action_operator_" + string(organization)
			project := tracker.ProjectID("project_" + string(organization))
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteAccessTokenGrant(t.Context(), credentialID, project) }); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct{ method, suffix, body string }{{http.MethodGet, "", ""}, {http.MethodPost, "", create}, {http.MethodPatch, suffix, patch}, {http.MethodDelete, suffix, ""}} {
				call(test.method, test.suffix, test.body, http.StatusForbidden)
			}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: credentialID, ProjectID: project})
			}); err != nil {
				t.Fatal(err)
			}
			call(http.MethodDelete, suffix, "", http.StatusNoContent)
			assertCommandOwner(0, "")
			call(http.MethodDelete, suffix, "", http.StatusNoContent)
			if err := json.Unmarshal(call(http.MethodGet, "", "", http.StatusOK), &listed); err != nil {
				t.Fatal(err)
			}
			if len(listed.Items) != 0 {
				t.Fatalf("deleted action still listed: %+v", listed)
			}
			if replay := call(http.MethodPost, "", create, http.StatusCreated); string(replay) != string(created) {
				t.Fatalf("deleted action receipt changed: %s", replay)
			}
			if err := json.Unmarshal(call(http.MethodGet, "", "", http.StatusOK), &listed); err != nil {
				t.Fatal(err)
			}
			if len(listed.Items) != 0 {
				t.Fatal("receipt replay recreated action")
			}

			cutoff := now.Add(receiptstore.ReplayRetention + time.Nanosecond)
			var receipts []receiptstore.Record
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				receipts, err = r.(receiptstore.RetentionReader).ReceiptExpiryPage(t.Context(), cutoff, 50)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(receipts) != 2 {
				t.Fatalf("eligible action receipts=%d want 2", len(receipts))
			}
			for _, receipt := range receipts {
				if receipt.ResponseReplayUntilNs != now.Add(receiptstore.ReplayRetention).UnixNano() {
					t.Fatalf("unexpected replay deadline: %d", receipt.ResponseReplayUntilNs)
				}
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					expired, err := w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), receipt, now.Add(receiptstore.ReplayRetention))
					if err != nil {
						return err
					}
					if expired {
						t.Fatal("receipt expired at inclusive boundary")
					}
					expired, err = w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), receipt, cutoff)
					if err == nil && !expired {
						t.Fatal("eligible receipt did not expire")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					stored, err := r.Receipt(t.Context(), receipt.ActorID, receipt.Operation, receipt.CommandKey)
					if err != nil {
						return err
					}
					if !stored.ResponseExpired || stored.RequestHash != receipt.RequestHash {
						t.Fatal("expiry lost idempotency tombstone")
					}
					_, err = eventstore.NewContent(service.runDetailStore()).Receipt(t.Context(), stored)
					if !errors.Is(err, receiptstore.ErrExpired) {
						t.Fatalf("expired content read: %v", err)
					}
					object, err := r.ArchiveObjectByReference(t.Context(), eventstore.Reference(receipt.PayloadObjectKey, receipt.PayloadSHA256, receipt.PayloadBytes, receipt.PayloadContentBytes))
					if err != nil {
						return err
					}
					var owners int
					if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2`, organization, object.ID).Scan(&owners); err != nil {
						return err
					}
					if owners != 0 {
						t.Fatalf("expired response owners=%d", owners)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				bytes, err := r.ArchiveBytes(t.Context())
				if err == nil && bytes != 0 {
					t.Fatalf("archive charge after expiry=%d", bytes)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			call(http.MethodPost, "", create, http.StatusGone)
			call(http.MethodPatch, suffix, patch, http.StatusGone)
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteAccessTokenGrant(t.Context(), credentialID, project) }); err != nil {
				t.Fatal(err)
			}
			call(http.MethodPost, "", create, http.StatusForbidden)
			call(http.MethodPatch, suffix, patch, http.StatusForbidden)
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: credentialID, ProjectID: project})
			}); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(call(http.MethodGet, "", "", http.StatusOK), &listed); err != nil {
				t.Fatal(err)
			}
			if len(listed.Items) != 0 {
				t.Fatal("expired replay recreated deleted action")
			}
			for i := 0; i < workspacesession.MaxActionsPerProject; i++ {
				call(http.MethodPost, "", fmt.Sprintf(`{"idempotency_key":"cap-%d","name":"Example","command":"true"}`, i), http.StatusCreated)
			}
			call(http.MethodPost, "", `{"idempotency_key":"over-cap","name":"Example","command":"true"}`, http.StatusUnprocessableEntity)
		})
	}
}

func TestPostgresProjectActionDeletionLifecycle(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC()
	details := &memoryRunDetails{}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_action_other"} {
		seedStartupOrganization(t, repository, organization)
		for _, sameID := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same_id_%t", organization, sameID), func(t *testing.T) {
				first := tracker.ProjectID(fmt.Sprintf("first_%t", sameID))
				second := tracker.ProjectID(fmt.Sprintf("second_%t", sameID))
				ref, err := details.StoreDetail(t.Context(), string(organization), []byte(fmt.Sprintf("command %t", sameID)))
				if err != nil {
					t.Fatal(err)
				}
				var object artifactstore.ArchiveObject
				write := func(fn func(tenantstore.Writer) error) {
					t.Helper()
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, fn); err != nil {
						t.Fatal(err)
					}
				}
				write(func(w tenantstore.Writer) error {
					for i, project := range []tracker.ProjectID{first, second} {
						if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native", CreatedAt: now}); err != nil {
							return err
						}
						id := fmt.Sprintf("action_%d", i)
						if sameID {
							id = "action_shared"
						}
						record := actionstore.Record{Action: workspacesession.Action{ID: id, OrganizationID: string(organization), ProjectID: string(project), Name: "Example", CreatedBy: "operator", Revision: 1, CreatedAt: now, UpdatedAt: now}, Command: ref}
						if err := w.(actionstore.Writer).CreateProjectAction(t.Context(), record); err != nil {
							return err
						}
						var err error
						object, err = w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: organization, ID: newNativeID("att"), Reference: ref, CreatedAt: now})
						if err != nil {
							return err
						}
						if err := w.AddArchiveOwner(t.Context(), artifactstore.ArchiveOwner{OrganizationID: organization, ArchiveID: object.ID, Kind: "project-action", ID: actionstore.ArchiveOwnerID(project, id), CreatedAt: now}); err != nil {
							return err
						}
					}
					return nil
				})
				assertOwners := func(want int, bytes int64) {
					t.Helper()
					var owners int
					if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2 AND owner_kind='project-action'`, organization, object.ID).Scan(&owners); err != nil {
						t.Fatal(err)
					}
					if owners != want {
						t.Fatalf("shared command owners=%d want %d", owners, want)
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						actual, err := r.ArchiveBytes(t.Context())
						if err == nil && actual != bytes {
							t.Fatalf("archive bytes=%d want %d", actual, bytes)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				assertOwners(2, ref.CompressedSize)
				write(func(w tenantstore.Writer) error { return w.DeleteProject(t.Context(), first) })
				assertOwners(1, ref.CompressedSize)
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.(actionstore.Reader).ProjectActions(t.Context(), first)
					if err != nil {
						return err
					}
					if len(rows) != 0 {
						t.Fatal("physical deletion retained action rows")
					}
					rows, err = r.(actionstore.Reader).ProjectActions(t.Context(), second)
					if err != nil {
						return err
					}
					if len(rows) != 1 {
						t.Fatal("physical deletion lost other project's action")
					}
					raw, err := details.ReadDetail(t.Context(), string(organization), rows[0].Command)
					if err == nil && len(raw) == 0 {
						t.Fatal("shared command unavailable")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				write(func(w tenantstore.Writer) error { return w.DeleteProject(t.Context(), second) })
				assertOwners(0, 0)
			})
		}
	}
}

func TestPostgresProjectActionsRollbackRefusesRetainedData(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC()
	details := &memoryRunDetails{}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_action_other"} {
		seedStartupOrganization(t, repository, organization)
		ref, err := details.StoreDetail(t.Context(), string(organization), []byte("retained command"))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			project := tracker.ProjectID("retained_project")
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Slug: "example", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.(actionstore.Writer).CreateProjectAction(t.Context(), actionstore.Record{Action: workspacesession.Action{ID: "retained_action", OrganizationID: string(organization), ProjectID: string(project), Name: "Example", CreatedBy: "operator", Revision: 1, CreatedAt: now, UpdatedAt: now}, Command: ref}); err != nil {
				return err
			}
			object, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: organization, ID: newNativeID("att"), Reference: ref, CreatedAt: now})
			if err != nil {
				return err
			}
			if err := w.AddArchiveOwner(t.Context(), artifactstore.ArchiveOwner{OrganizationID: organization, ArchiveID: object.ID, Kind: "project-action", ID: actionstore.ArchiveOwnerID(project, "retained_action"), CreatedAt: now}); err != nil {
				return err
			}
			return w.RetireProject(t.Context(), project, 1, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_action_other"} {
		var owners int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM archive_object_owners WHERE organization_id=$1 AND owner_kind='project-action'`, organization).Scan(&owners); err != nil {
			t.Fatal(err)
		}
		if owners != 1 {
			t.Fatalf("soft retirement retained owners=%d want 1", owners)
		}
	}
	conn, err := admin.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(ctx, `RESET ROLE`); err != nil {
			t.Error(err)
		}
	}()
	var visible int
	if err := conn.QueryRowContext(t.Context(), `SELECT count(*) FROM project_actions`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("migration role sees tenant rows without scope: %d", visible)
	}
	raw, err := os.ReadFile("postgres/migrations/20261104002716_project_actions.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("missing Down migration")
	}
	tx, err := conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, rollbackErr := tx.ExecContext(t.Context(), down)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if rollbackErr == nil || !strings.Contains(rollbackErr.Error(), "project action records require an explicit retained-data migration") {
		t.Fatalf("rollback refusal: %v", rollbackErr)
	}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_action_other"} {
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			project, err := r.Project(t.Context(), "retained_project")
			if err != nil {
				return err
			}
			if project.DeletedAt == nil {
				t.Fatal("soft retirement did not retain retired project")
			}
			rows, err := r.(actionstore.Reader).ProjectActions(t.Context(), "retained_project")
			if err != nil {
				return err
			}
			if len(rows) != 1 {
				t.Fatal("rollback lost retained action")
			}
			bytes, err := r.ArchiveBytes(t.Context())
			if err == nil && bytes != rows[0].Command.CompressedSize {
				t.Fatal("rollback changed archive usage")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
