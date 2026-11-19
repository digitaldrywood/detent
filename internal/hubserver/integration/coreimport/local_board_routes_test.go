package coreimport_test

import (
	"database/sql"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalBoardImportRefusals(t *testing.T) {
	t.Parallel()
	records := importRouteRecords()
	hub := hubfixture.Open(t, hubfixture.Config{})
	f := importRouteProject(t, hub, "Import refusals")
	ungranted := importRouteProject(t, hub, "Ungranted import")
	worker := hubtest.Credential(t, hub.Repository, f.Organization(), hubtest.CredentialOptions{
		Scope: string(hubapi.APIScopeWorker), Projects: []tracker.ProjectID{f.Project.ID}, Now: hub.Now(),
	})
	for _, test := range []struct {
		name, token string
		record      localboard.Record
		status      int
	}{
		{"worker", worker.Token, records[0], http.StatusForbidden},
		{"missing grant", ungranted.Token, records[0], http.StatusNotFound},
		{"missing prerequisite", f.Token, records[1], http.StatusUnprocessableEntity},
		{"invalid timestamp", f.Token, func() localboard.Record {
			r := records[0]
			r.CreatedAt = time.Time{}
			r.Title = "title-secret"
			r.Body = "body-secret"
			return r
		}(), http.StatusUnprocessableEntity},
		{"invalid comment identity", f.Token, func() localboard.Record {
			r := records[0]
			r.Comments = []localboard.Comment{{Body: "body-secret", CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}}
			return r
		}(), http.StatusUnprocessableEntity},
		{"invalid content", f.Token, func() localboard.Record {
			r := records[0]
			r.Title = strings.Repeat("title-secret", 100)
			r.Body = "body-secret"
			return r
		}(), http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := localBoardCounts(t, hub.Admin)
			response := hub.Request(t, http.MethodPost, f.Base+"/local-board-import", test.token, localboard.Request{Record: test.record})
			hubfixture.RequireStatus(t, response, test.status)
			if strings.Contains(response.Body.String(), "title-secret") || strings.Contains(response.Body.String(), "body-secret") {
				t.Fatal("error exposed issue content")
			}
			if after := localBoardCounts(t, hub.Admin); !reflect.DeepEqual(before, after) {
				t.Fatal("refused import wrote data")
			}
		})
	}
	hubfixture.RequireStatus(t, hub.Request(t, http.MethodPost, f.Base+"/local-board-import", f.Token, localboard.Request{Record: records[0]}), http.StatusOK)
	changed := records[0]
	changed.Body += " changed"
	before := localBoardCounts(t, hub.Admin)
	hubfixture.RequireStatus(t, hub.Request(t, http.MethodPost, f.Base+"/local-board-import", f.Token, localboard.Request{Record: changed}), http.StatusUnprocessableEntity)
	if after := localBoardCounts(t, hub.Admin); !reflect.DeepEqual(before, after) {
		t.Fatalf("changed pinned source wrote rows: %v -> %v", before, after)
	}
	t.Run("prerequisite in another authorized project", func(t *testing.T) {
		other := importRouteProject(t, hub, "Prerequisite isolation")
		if other.Project.OrganizationID != f.Project.OrganizationID || other.Project.ID == f.Project.ID {
			t.Fatal("fixture requires distinct projects in the same organization")
		}
		response := hub.Request(t, http.MethodGet, other.Base, other.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		admin := hub.Admin
		counts := func() [2][2]int {
			var rows [2][2]int
			for i, fixture := range []hubfixture.Native{f, other} {
				if err := admin.QueryRowContext(t.Context(), `SELECT
					(SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2),
					(SELECT count(*) FROM local_board_imports WHERE organization_id=$1 AND project_id=$2)`,
					fixture.Project.OrganizationID, fixture.Project.ID).Scan(&rows[i][0], &rows[i][1]); err != nil {
					t.Fatal(err)
				}
			}
			return rows
		}
		before := counts()
		if before[0][1] != 1 || before[1][1] != 0 {
			t.Fatalf("prerequisite mappings before import = %v", before)
		}
		allBefore := localBoardCounts(t, hub.Admin)
		response = hub.Request(t, http.MethodPost, other.Base+"/local-board-import", other.Token, localboard.Request{Record: records[1]})
		hubfixture.RequireStatus(t, response, http.StatusUnprocessableEntity)
		if after := counts(); after != before {
			t.Fatalf("foreign prerequisite changed project rows: %v -> %v", before, after)
		}
		if after := localBoardCounts(t, hub.Admin); !reflect.DeepEqual(allBefore, after) {
			t.Fatalf("foreign prerequisite wrote rows: %v -> %v", allBefore, after)
		}
	})

}

func TestLocalBoardImportRollsBackAtSourceMappingWrite(t *testing.T) {
	t.Parallel()
	hub := hubfixture.Open(t, hubfixture.Config{})
	f := importRouteProject(t, hub, "Import rollback")
	records := importRouteRecords()
	hubfixture.RequireStatus(t, hub.Request(t, http.MethodPost, f.Base+"/local-board-import", f.Token, localboard.Request{Record: records[0]}), http.StatusOK)
	admin := hub.Admin
	if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION refuse_local_board_mapping() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'mapping write refused'; END$$; CREATE TRIGGER refuse_local_board_mapping BEFORE INSERT ON local_board_imports FOR EACH ROW EXECUTE FUNCTION refuse_local_board_mapping()`); err != nil {
		t.Fatal(err)
	}
	before := localBoardCounts(t, hub.Admin)
	response := hub.Request(t, http.MethodPost, f.Base+"/local-board-import", f.Token, localboard.Request{Record: records[1]})
	hubfixture.RequireStatus(t, response, http.StatusInternalServerError)
	if after := localBoardCounts(t, hub.Admin); !reflect.DeepEqual(before, after) {
		t.Fatalf("failed final mapping left partial item, comment, dependency, version, event, search or archive rows: %v -> %v", before, after)
	}
}

func importRouteRecords() []localboard.Record {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	updated := created.AddDate(0, 0, 1)
	records := []localboard.Record{
		{Key: localboard.SourceKey("example-board", "default", "local_sqlite", "z-first"), Kind: "local_sqlite", SourceID: "z-first", Number: 10, Title: "Example z-first", Body: "Prerequisite body", State: "todo", Author: "source-author", CreatedAt: created, UpdatedAt: updated, Priority: new(2)},
		{Key: localboard.SourceKey("example-board", "default", "local_sqlite", "a-second"), Kind: "local_sqlite", SourceID: "a-second", Number: 20, Title: "Example a-second", Body: "Dependent body", State: "Retired lane", Author: "source-author", CreatedAt: created, UpdatedAt: updated, Comments: []localboard.Comment{{ID: "1", Body: "Edited comment", CreatedAt: created, UpdatedAt: updated}}},
	}
	records[1].Dependencies = []string{records[0].Key}
	return records
}

func localBoardCounts(t *testing.T, admin *sql.DB) []int {
	t.Helper()
	counts := []int{}
	for _, query := range []string{"SELECT count(*) FROM issues", "SELECT count(*) FROM native_comments", "SELECT count(*) FROM local_board_imports", "SELECT count(*) FROM native_commands", "SELECT count(*) FROM issue_relationships", "SELECT (SELECT count(*) FROM collaboration_versions)+(SELECT count(*) FROM collaboration_events)+(SELECT count(*) FROM native_attempt_events)", "SELECT count(*) FROM collaboration_events", "SELECT count(*) FROM archive_objects"} {
		var count int
		if err := admin.QueryRowContext(t.Context(), query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, count)
	}
	return counts
}

func importRouteProject(t *testing.T, h *hubfixture.Hub, name string) hubfixture.Native {
	t.Helper()
	states := hubrecords.NativeTriageStates(append(hubfixture.States(), tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}}))
	project := hubfixture.NewNativeWith(t, h, "", hubtest.ProjectOptions{Name: name, States: states, RequireDependencies: true})
	if err := h.Repository.WriteTenant(t.Context(), project.Organization(), corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), project.Project.ID)
		if err != nil {
			return err
		}
		record.Settings.DuplicateChallenge = false
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return project
}
