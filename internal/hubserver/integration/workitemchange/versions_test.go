package workitemchange_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeDetailEmptyCollectionsAreArrays(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	for _, id := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(id), func(t *testing.T) {
			h.EnsureOrganization(t, id)
			projects := "/api/v2/organizations/" + string(id) + "/projects"
			response := h.Request(t, http.MethodPost, projects, hubfixture.AdminToken, map[string]any{"idempotency_key": "project", "name": "Example project", "states": hubfixture.States()})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var project tracker.NativeProject
			hubfixture.Decode(t, response, &project)
			base := projects + "/" + string(project.ID)
			hubfixture.ApprovePolicy(t, h, base+"/policy", hubfixture.Policy())
			response = h.Request(t, http.MethodPost, base+"/work-items", hubfixture.AdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue"}, Title: "Example issue", Body: "## Acceptance criteria\nThe issue is served.\n## Must not break\nOther tenants.\n## How we know it worked\nRead the issue."})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			hubfixture.Decode(t, response, &issue)
			path := base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response = h.Request(t, http.MethodPost, path, hubfixture.AdminToken, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Example Change"})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			hubfixture.Decode(t, response, &change)
			path += "/" + change.ID
			for _, published := range []bool{false, true} {
				if published {
					input := hubfixture.ChangeInput()
					input.Artifacts = nil
					response = h.Request(t, http.MethodPost, path+"/versions", hubfixture.AdminToken, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish"}, ChangeVersionInput: input})
					hubfixture.RequireStatus(t, response, http.StatusOK)
				}
				for _, view := range []string{"", "?view=bounded"} {
					response = h.Request(t, http.MethodGet, path+view, hubfixture.AdminToken, nil)
					hubfixture.RequireStatus(t, response, http.StatusOK)
					var payload json.RawMessage
					hubfixture.Decode(t, response, &payload)
					if view != "" {
						var envelope struct{ Detail json.RawMessage }
						if err := json.Unmarshal(payload, &envelope); err != nil {
							t.Fatal(err)
						}
						payload = envelope.Detail
					}
					var typed tracker.ChangeDetail
					if err := json.Unmarshal(payload, &typed); err != nil {
						t.Fatal(err)
					}
					if typed.Change.OrganizationID != id || len(typed.Change.LinkedIssues) != 1 || typed.Change.LinkedIssues[0] != issue.WorkItemID || typed.Summary.Messages == nil {
						t.Errorf("view=%q invalid Change identity or collection shape: %+v", view, typed)
					}
					var detail struct {
						Checks, Reviews, Discussion json.RawMessage
						Versions                    []struct {
							Artifacts, Checks json.RawMessage
							ReviewPolicy      struct {
								RequiredChecks json.RawMessage `json:"required_checks"`
							} `json:"review_policy"`
						}
					}
					if err := json.Unmarshal(payload, &detail); err != nil {
						t.Fatal(err)
					}
					for name, raw := range map[string]json.RawMessage{"checks": detail.Checks, "reviews": detail.Reviews, "discussion": detail.Discussion} {
						if string(raw) != "[]" {
							t.Errorf("published=%t %s=%s, want []", published, name, raw)
						}
					}
					if !published {
						if detail.Versions == nil || len(detail.Versions) != 0 {
							t.Errorf("draft versions=%+v, want []", detail.Versions)
						}
						continue
					}
					if len(detail.Versions) != 1 {
						t.Fatalf("versions=%d, want 1", len(detail.Versions))
					}
					version := detail.Versions[0]
					for name, raw := range map[string]json.RawMessage{"artifacts": version.Artifacts, "checks": version.Checks, "review_policy.required_checks": version.ReviewPolicy.RequiredChecks} {
						if string(raw) != "[]" {
							t.Errorf("version %s=%s, want []", name, raw)
						}
					}
				}
			}
			response = h.Request(t, http.MethodGet, path, hubfixture.AdminToken, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var published tracker.ChangeDetail
			hubfixture.Decode(t, response, &published)
			for _, test := range []struct {
				name, version string
			}{
				{name: "whole Change"},
				{name: "current version", version: published.Change.CurrentVersion},
			} {
				t.Run("recovery discussion "+test.name, func(t *testing.T) {
					body := "Preserve exact feedback for " + string(id) + " " + test.name
					response := h.Request(t, http.MethodPost, path+"/discussion", hubfixture.AdminToken, tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "discuss-" + test.name}, VersionID: test.version, Body: body})
					hubfixture.RequireStatus(t, response, http.StatusOK)
					response = h.Request(t, http.MethodGet, base+"/work-items/"+string(issue.WorkItemID)+"?view=recovery", hubfixture.AdminToken, nil)
					hubfixture.RequireStatus(t, response, http.StatusOK)
					var recovery tracker.NativeRecovery
					hubfixture.Decode(t, response, &recovery)
					if recovery.ChangeDetail == nil || !slices.ContainsFunc(recovery.ChangeDetail.Discussion, func(discussion tracker.ChangeDiscussion) bool {
						return discussion.Body == body && discussion.VersionID == test.version
					}) {
						t.Fatalf("source recovery lost current Change feedback: %+v", recovery.ChangeDetail)
					}
					response = h.Request(t, http.MethodGet, path, hubfixture.AdminToken, nil)
					hubfixture.RequireStatus(t, response, http.StatusOK)
					var complete tracker.ChangeDetail
					hubfixture.Decode(t, response, &complete)
					if !reflect.DeepEqual(recovery.ChangeDetail.Versions, complete.Versions) {
						t.Fatalf("recovery source version differs: expected=%+v actual=%+v", complete.Versions, recovery.ChangeDetail.Versions)
					}
				})
			}
		})
	}
}
