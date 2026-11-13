package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkItemContentChargesCollaborationBytesPerTenant(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	now := time.Now().UTC()
	service := &Service{echo: echo.New(), notifications: newNotificationBroker(), boardRefresh: make(chan struct{}, 1), database: &database{postgres: repository, now: func() time.Time { return now }}, secretRewrapReady: make(chan struct{}), config: Config{Logger: discardLogger(), TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }}}
	service.database.hostedRuntime.Store(&hostedplans.RuntimeConfig{Plans: hostedplans.HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 10}})
	service.registerNativeRoutes(service.echo)
	organizations := []tracker.OrganizationID{"org_charge_first", "org_charge_second"}
	project := func(organization tracker.OrganizationID) tracker.ProjectID {
		return tracker.ProjectID("project_" + string(organization))
	}
	for _, organization := range organizations {
		seedStartupOrganization(t, repository, organization)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project(organization), Name: "Example", Profile: "native", CreatedAt: now}, States: nativeFixtureStates()}); err != nil {
				return fmt.Errorf("project creation: %w", err)
			}
			token := string(organization) + "_write"
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: token, Name: "Example member", Hash: apikey.HashToken(token), Scope: "operator", KeyScope: "write", NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
				return fmt.Errorf("credential: %w", err)
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: token, ProjectID: project(organization)}); err != nil {
				return fmt.Errorf("token grant: %w", err)
			}
			plan := billingstore.PlanReference{ID: "plan_" + string(organization), Version: 1}
			if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: organization, PlanReference: plan, Name: "Example plan", Features: []string{"collaboration"}, Allowances: map[string]int64{"api_mutations": 1000, "ingested_events": 1000, "collaboration_bytes": 1 << 30}}); err != nil {
				return fmt.Errorf("billing plan: %w", err)
			}
			return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization, Base: plan}, 0)
		}); err != nil {
			t.Fatal(err)
		}
		seedNativeFixturePolicy(t, service, string(organization)+"/"+string(project(organization)))
	}
	content := eventstore.NewContent(service.runDetailStore())
	counter := func(organization tracker.OrganizationID) int64 {
		t.Helper()
		var used int64
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			used, err = r.CollaborationBytes(t.Context())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return used
	}
	recount := func(organization tracker.OrganizationID, item tracker.NativeWorkItemID, issueRevisions int64, comment string, commentRevisions int64) int64 {
		t.Helper()
		var total int64
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			for _, record := range []struct {
				id        string
				revisions int64
			}{{string(item), issueRevisions}, {comment, commentRevisions}} {
				for revision := int64(1); revision <= record.revisions; revision++ {
					version, err := r.CollaborationVersion(t.Context(), record.id, revision)
					if err != nil {
						return err
					}
					raw, err := content.CollaborationVersion(t.Context(), version)
					if err != nil {
						return err
					}
					total += int64(len(raw))
					if revision != record.revisions {
						continue
					}
					if record.id == comment {
						var current tracker.NativeComment
						if err := json.Unmarshal(raw, &current); err != nil {
							return err
						}
						total += int64(len(current.Body))
						continue
					}
					var current tracker.NativeIssue
					if err := json.Unmarshal(raw, &current); err != nil {
						return err
					}
					labels, err := json.Marshal(current.Labels)
					if err != nil {
						return err
					}
					assignees, err := json.Marshal(current.Assignees)
					if err != nil {
						return err
					}
					total += int64(len(current.Title) + len(current.Body) + len(labels) + len(assignees))
				}
			}
			events, err := r.Events(t.Context(), project(organization), item, 0, 100)
			if err != nil {
				return err
			}
			if int64(len(events)) != issueRevisions+commentRevisions {
				return fmt.Errorf("events=%d, want one per version", len(events))
			}
			for _, row := range events {
				event, err := content.Event(t.Context(), row)
				if err != nil {
					return err
				}
				data, err := json.Marshal(event.Data)
				if err != nil {
					return err
				}
				actor, err := json.Marshal(event.Actor)
				if err != nil {
					return err
				}
				total += int64(len(data) + len(actor))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return total
	}
	want := map[tracker.OrganizationID]int64{}
	for index, organization := range organizations {
		token := string(organization) + "_write"
		base := "/api/v2/organizations/" + string(organization) + "/projects/" + string(project(organization)) + "/work-items"
		response := performHubAPIRequest(t, service, http.MethodPost, base, token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create"}, Title: "Charged issue", Body: nativeContractTestBody, State: "Todo"})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		for step, size := range []int{4096 * (index + 1), 512} {
			body := strings.Repeat("x", size) + "\n" + nativeContractTestBody
			response = performHubAPIRequest(t, service, http.MethodPatch, base+"/"+string(issue.WorkItemID), token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("body-%d", step)}, ExpectedRevision: issue.Revision, Body: &body})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
		}
		response = performHubAPIRequest(t, service, http.MethodPost, base+"/"+string(issue.WorkItemID)+"/comments", token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: strings.Repeat("c", 2048*(index+1))})
		requireNativeStatus(t, response, http.StatusOK)
		var comment tracker.NativeComment
		decodeHubResponse(t, response, &comment)
		response = performHubAPIRequest(t, service, http.MethodPatch, base+"/"+string(issue.WorkItemID)+"/comments/"+comment.ID, token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment-edit"}, ExpectedRevision: comment.Revision, Body: "shorter"})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &comment)
		want[organization] = recount(organization, issue.WorkItemID, int64(issue.Revision), comment.ID, int64(comment.Revision))
		for _, checked := range organizations {
			if got := counter(checked); got != want[checked] {
				t.Fatalf("after %s writes, %s collaboration bytes counter=%d, raw recount=%d", organization, checked, got, want[checked])
			}
		}
	}
	if want[organizations[0]] == 0 || want[organizations[0]] == want[organizations[1]] {
		t.Fatalf("recounts do not distinguish tenants: %v", want)
	}
	replay := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(organizations[0])+"/projects/"+string(project(organizations[0]))+"/work-items", string(organizations[0])+"_write", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create"}, Title: "Charged issue", Body: nativeContractTestBody, State: "Todo"})
	requireNativeStatus(t, replay, http.StatusOK)
	if got := counter(organizations[0]); got != want[organizations[0]] {
		t.Fatalf("replayed create charged again: counter=%d, want %d", got, want[organizations[0]])
	}
}
