package coreimport_test

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport/coreimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeColumnsSelectRecoveryComments(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	for _, state := range []string{"typed", "typed author changed", "unconverted"} {
		t.Run(state, func(t *testing.T) {
			f := hubfixture.NewNative(t, h, hubfixture.Primary, "recovery comments "+state)
			var items []tracker.NativeWorkItemID
			if state == "unconverted" {
				fixture := coreimporttest.WorkItems(t, os.DirFS("../../../.."), f.Project.OrganizationID, f.Project.ID, false, coreimporttest.WorkItemBody())
				fixture.Records["native_comments"] = nil
				kept := fixture.Records["collaboration_versions"][:0]
				for _, row := range fixture.Records["collaboration_versions"] {
					var id string
					if err := row.Decode(map[string]any{"record_id": &id}); err != nil {
						t.Fatal(err)
					}
					if strings.HasPrefix(id, "wi_core_import") {
						row.SourceRowID = int64(len(kept) + 1)
						kept = append(kept, row)
					}
				}
				fixture.Records["collaboration_versions"] = kept
				at := parseTestTime(t, "2026-10-01T00:00:00Z")
				for _, id := range []tracker.NativeWorkItemID{"wi_core_import", "wi_core_import_second"} {
					for i, kinds := range [][2]string{{"runner", ""}, {"human", ""}, {"runner", "human"}, {"runner", ""}} {
						comment := changeColumnComment(f, id, fmt.Sprintf("%s_comment_%d", id, i), int64(i+1), kinds, at)
						var editor any
						if comment.EditedBy != nil {
							editor = string(historyTestJSON(t, comment.EditedBy))
						}
						fixture.Add("native_comments", map[string]any{"id": comment.ID, "organization_id": f.Project.OrganizationID, "project_id": f.Project.ID, "work_item_id": id, "revision": 1, "sequence": i + 1, "body": comment.Body, "actor_json": string(historyTestJSON(t, comment.Actor)), "edited_by_json": editor, "change_actor_kind": nil, "change_edited_by_kind": nil, "created_at": hubapi.FormatHubTime(at), "updated_at": hubapi.FormatHubTime(at)})
						fixture.Add("collaboration_versions", map[string]any{"organization_id": f.Project.OrganizationID, "project_id": f.Project.ID, "work_item_id": id, "record_id": comment.ID, "revision": 1, "encoding": "snapshot", "record_json": string(historyTestJSON(t, comment))})
					}
				}
				source := coreimport.CoreWorkItemImportSource{BundlePath: fixture.Write(), OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, Objects: f.Hub.Config.RunDetails}
				if _, err := coreimport.ImportCoreWorkItems(t.Context(), h.Repository, source); err != nil {
					t.Fatal(err)
				}
				items = []tracker.NativeWorkItemID{"wi_core_import", "wi_core_import_second"}
			} else {
				items = []tracker.NativeWorkItemID{f.Create(t, "first comments").WorkItemID, f.Create(t, "second comments").WorkItemID}
			}
			for _, itemID := range items {
				ids := make([]string, 4)
				for i, kinds := range [][2]string{{"runner", ""}, {"human", ""}, {"runner", "human"}, {"runner", ""}} {
					ids[i] = fmt.Sprintf("%s_comment_%d", itemID, i)
					if state != "unconverted" {
						changeColumnWriteComment(t, f, changeColumnComment(f, itemID, ids[i], int64(i+1), kinds, f.Hub.Now()), state == "typed author changed" && i == 1)
					}
				}
				want := []string{ids[1], ids[2], ids[3]}
				if state == "typed author changed" {
					want = []string{ids[2], ids[3]}
				}
				path := f.Base + "/work-items/" + string(itemID) + "?view=recovery&projection=bounded"
				response := h.Request(t, http.MethodGet, path, f.Token, nil)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var recovery tracker.NativeRecovery
				hubfixture.Decode(t, response, &recovery)
				got := make([]string, len(recovery.Discussion))
				for i, comment := range recovery.Discussion {
					got[i] = comment.ID
				}
				if recovery.Issue.WorkItemID != itemID || !slices.Equal(got, want) {
					t.Fatalf("%s recovery for %s: issue=%s comments=%v want=%v", state, itemID, recovery.Issue.WorkItemID, got, want)
				}
			}
		})
	}
}

func changeColumnComment(f hubfixture.Native, item tracker.NativeWorkItemID, id string, sequence int64, kinds [2]string, at time.Time) tracker.NativeComment {
	comment := tracker.NativeComment{OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, WorkItemID: item, ID: id, Revision: 1, Sequence: sequence, Body: "comment", Actor: tracker.Actor{Kind: kinds[0], PrincipalID: "author"}, CreatedAt: at, UpdatedAt: at}
	if kinds[1] != "" {
		comment.EditedBy = &tracker.Actor{Kind: kinds[1], PrincipalID: "editor"}
	}
	return comment
}

func changeColumnWriteComment(t *testing.T, f hubfixture.Native, comment tracker.NativeComment, tamper bool) {
	t.Helper()
	content := eventstore.NewContent(f.Hub.Config.RunDetails)
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, WorkItemID: comment.WorkItemID, RecordID: comment.ID, Revision: 1}, historyTestJSON(t, comment))
	if err != nil {
		t.Fatal(err)
	}
	fields, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.Project.OrganizationID}, historyTestJSON(t, comment.Body))
	if err != nil {
		t.Fatal(err)
	}
	row := corestore.Comment{OrganizationID: f.Project.OrganizationID, ProjectID: f.Project.ID, WorkItemID: comment.WorkItemID, ID: comment.ID, Revision: 1, Sequence: comment.Sequence, BodyObjectKey: fields.PayloadObjectKey, ActorKind: comment.Actor.Kind, ActorPrincipalID: comment.Actor.PrincipalID, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt}
	if comment.EditedBy != nil {
		row.EditedByKind, row.EditedByPrincipalID = &comment.EditedBy.Kind, &comment.EditedBy.PrincipalID
	}
	if tamper {
		row.ActorKind = "runner"
	}
	if err := f.Hub.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateComment(t.Context(), row); err != nil {
			return err
		}
		_, err := w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
