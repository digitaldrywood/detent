package conversation_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTeamThreadScopeReferenceAccess(t *testing.T) {
	t.Parallel()
	t.Run("reference resource authorization", func(t *testing.T) {
		v := hubfixture.OpenConversationHostedEntry(t)
		if err := v.Repository.WriteTenant(t.Context(), v.Primary, corestore.Serving, func(w tenantstore.Writer) error {
			project, err := w.ProjectRecord(t.Context(), v.Project)
			if err != nil {
				return err
			}
			if slices.ContainsFunc(project.States, func(state tracker.NativeState) bool { return state.Name == "Triage" }) {
				return nil
			}
			project.States = append(project.States, tracker.NativeState{Name: "Triage", Transitions: []string{"Todo"}})
			_, err = w.UpdateProjectRecord(t.Context(), project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		roles := map[string]string{"owner": "owner", "reporter": "reporter", "tester": "reporter", "member": "member"}
		users := map[string]hubfixture.HostedUser{}
		for _, name := range []string{"owner", "reporter", "tester", "member"} {
			users[name] = v.User(t, "thread-"+name, roles[name], name+"@example.test", "write", "")
		}
		request := func(t *testing.T, name, method, path string, input any) *httptest.ResponseRecorder {
			t.Helper()
			return v.BrowserRequest(t, users[name], roles[name], method, path, input)
		}
		base := hubfixture.OrganizationBase(v.Primary) + "/threads"
		record := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "private-report.png", ContentType: "image/png", Size: 100, Width: 1, Height: 1, SHA256: strings.Repeat("a", 64)}
		r := request(t, "tester", http.MethodPost, v.Base+"/attachment-metadata", record)
		hubfixture.RequireStatus(t, r, http.StatusCreated)
		hubfixture.Decode(t, r, &record)
		r = request(t, "tester", http.MethodPost, v.Base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "private-report"}, Type: "bug", Title: "Private report", Body: `## Steps to reproduce
Open the app and save.
## Expected
The change saves.
## Actual
The change disappears.
## Where
Settings screen.
## Environment
Firefox on Linux.` + "\n![Image](attachment:" + record.ID + ")"})
		hubfixture.RequireStatus(t, r, http.StatusOK)
		var issue tracker.NativeIssue
		hubfixture.Decode(t, r, &issue)
		r = request(t, "member", http.MethodPost, v.Base+"/work-items/"+string(issue.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "private-change"}, Title: "Private change", Body: "Change details"})
		hubfixture.RequireStatus(t, r, http.StatusOK)
		var change tracker.ChangeRequest
		hubfixture.Decode(t, r, &change)
		for _, reference := range []operatortool.ThreadReference{{Kind: "attachment", ID: record.ID}, {Kind: "issue", ID: string(issue.WorkItemID)}, {Kind: "change", ID: change.ID}} {
			t.Run(reference.Kind, func(t *testing.T) {
				input := operatortool.ThreadArguments{Scope: "general", Title: "Shared discussion", References: []operatortool.ThreadReference{reference}}
				r := request(t, "owner", http.MethodPost, base, input)
				hubfixture.RequireStatus(t, r, http.StatusCreated)
				var thread threadstore.Thread
				hubfixture.Decode(t, r, &thread)
				post := operatortool.ThreadArguments{Kind: "note", Body: "Shared note", References: input.References}
				hubfixture.RequireStatus(t, request(t, "owner", http.MethodPost, base+"/"+thread.ID+"/posts", post), http.StatusCreated)
				for _, tc := range []struct {
					viewer   string
					readable bool
				}{{"reporter", false}, {"tester", true}, {"member", true}} {
					t.Run(tc.viewer, func(t *testing.T) {
						want := 0
						status := http.StatusNotFound
						if tc.readable {
							want, status = 1, http.StatusCreated
						}
						for _, target := range []string{base + "/" + thread.ID, base} {
							r := request(t, tc.viewer, http.MethodGet, target, nil)
							hubfixture.RequireStatus(t, r, http.StatusOK)
							var references []threadstore.Reference
							if target == base {
								var page threadstore.Page
								hubfixture.Decode(t, r, &page)
								found := false
								for _, listed := range page.Threads {
									if listed.ID == thread.ID {
										references, found = listed.References, true
									}
								}
								if !found {
									t.Fatal("shared thread missing from list")
								}
							} else {
								var read threadstore.Read
								hubfixture.Decode(t, r, &read)
								references = read.Thread.References
								if len(read.Posts) == 0 {
									t.Fatal("shared post missing")
								}
								for _, listed := range read.Posts {
									if len(listed.References) != want {
										t.Errorf("post references=%+v, want %d", listed.References, want)
									}
								}
							}
							if len(references) != want {
								t.Errorf("references=%+v, want %d", references, want)
							}
							if !tc.readable && strings.Contains(r.Body.String(), record.Name) {
								t.Error("unreadable attachment name disclosed")
							}
							if tc.readable && reference.Kind == "attachment" && (len(references) != 1 || references[0].Label != record.Name) {
								t.Errorf("attachment filename missing: %+v", references)
							}
						}
						hubfixture.RequireStatus(t, request(t, tc.viewer, http.MethodPost, base, input), status)
						hubfixture.RequireStatus(t, request(t, tc.viewer, http.MethodPost, base+"/"+thread.ID+"/posts", post), status)
					})
				}
			})
		}
	})
}
