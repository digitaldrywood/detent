package projectadmin_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectGitHubBatchUsesPostgresRecords(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	projects := []hubfixture.Native{
		hubfixture.NewNative(t, h, h.Primary, "First intake project"),
		hubfixture.NewNative(t, h, h.Primary, "Second intake project"),
	}
	type batchView struct {
		Batch    *tracker.GitHubBatch  `json:"batch"`
		Lanes    []tracker.NativeState `json:"lanes"`
		Progress struct {
			Discovered int `json:"discovered"`
			Total      int `json:"total"`
			Completed  int `json:"completed"`
			Pending    int `json:"pending"`
		} `json:"progress"`
	}
	read := func(t *testing.T, project hubfixture.Native) batchView {
		t.Helper()
		response := h.Request(t, http.MethodGet, project.Base+"/onboarding/issue-intake", h.AdminToken(), nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var view batchView
		hubfixture.Decode(t, response, &view)
		return view
	}
	if empty := read(t, projects[0]); empty.Batch != nil || len(empty.Lanes) == 0 {
		t.Fatalf("empty project intake view = %+v", empty)
	}
	for _, project := range projects {
		batch := tracker.GitHubBatch{
			ID: "batch_" + string(project.Project.ID), Revision: 1, Status: "ready", HubOwned: true,
			Page:  tracker.GitHubDiscoveryPage{Total: 2, Issues: []tracker.GitHubIssuePreview{{Number: 1, Body: "private body", Snapshot: &tracker.GitHubIssueSnapshot{}}}},
			Items: []tracker.GitHubBatchItem{{Number: 1, Status: "completed"}, {Number: 2, Status: "pending"}},
		}
		raw, err := json.Marshal(batch)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := eventstore.NewContent(h.Config.RunDetails).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: h.Primary}, raw)
		if err != nil {
			t.Fatal(err)
		}
		record := projectstore.IntakeBatch{ProjectID: project.Project.ID, ID: batch.ID, Revision: batch.Revision, Status: batch.Status, HubOwned: batch.HubOwned, Payload: payload}
		save := func() error {
			return h.Repository.WriteTenant(t.Context(), h.Primary, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(projectstore.IntakeBatchWriter).SaveProjectIntakeBatch(t.Context(), record, 0)
			})
		}
		if err := save(); err != nil {
			t.Fatal(err)
		}
		if project.Project.ID == projects[0].Project.ID {
			if err := save(); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("stale batch write = %v", err)
			}
		}
	}
	for _, project := range projects {
		actual := read(t, project)
		if actual.Batch == nil || actual.Batch.ID != "batch_"+string(project.Project.ID) || len(actual.Batch.Page.Issues) != 1 {
			t.Fatalf("project %s intake batch = %+v", project.Project.ID, actual.Batch)
		}
		if issue := actual.Batch.Page.Issues[0]; issue.Body != "" || issue.Snapshot != nil {
			t.Fatalf("project %s preview retained private content", project.Project.ID)
		}
		if actual.Progress.Discovered != 1 || actual.Progress.Total != 2 || actual.Progress.Completed != 1 || actual.Progress.Pending != 1 {
			t.Fatalf("project %s intake progress = %+v", project.Project.ID, actual.Progress)
		}
	}
}
