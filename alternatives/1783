package hubserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresHostedCheckAuthenticationWithoutLegacyDatabase(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := &Service{database: &database{}, config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }, Hosted: &HostedConfig{}}}
	content := eventstore.NewContent(service.runDetailStore())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			service.config.Hosted.OrganizationID = string(org)
			token := string(org) + "_check"
			expires := now.Add(time.Hour)
			rules := tracker.ChangeReviewPolicy{ID: "review", PolicyID: hubTestPolicy().ID, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: "check", Source: "independent"}}}
			row, err := content.PrepareReviewPolicy(t.Context(), eventstore.ReviewPolicy{OrganizationID: org, ProjectID: "project", PolicyRevisionID: hubTestPolicy().ID, Revision: 1, UpdatedBy: "owner", UpdatedAt: now}, rules)
			if err != nil {
				t.Fatal(err)
			}
			path := "/api/v2/organizations/" + string(org) + "/projects/project/work-items/item/changes/change/versions/version/checks"
			version := tracker.ChangeVersion{ID: "version", ChangeID: "change", Number: 1, ChangeVersionInput: changeTestInput(), Policy: hubTestPolicy(), Checks: []tracker.ChangeCheckExpectation{{ChangeCheckSpec: rules.RequiredChecks[0], CheckRunID: "check-run"}}, CreatedAt: now}
			request := changeTestResult(version)
			issueRow, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "project", WorkItemID: "item", RecordID: "item", Revision: 1}, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			changeRaw, err := json.Marshal(tracker.ChangeRequest{OrganizationID: org, ProjectID: "project", WorkItemID: "item", ID: "change", Title: "Example", Revision: 1, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			changeRow, err := content.PrepareChange(t.Context(), org, changeRaw)
			if err != nil {
				t.Fatal(err)
			}
			versionRaw, err := json.Marshal(version)
			if err != nil {
				t.Fatal(err)
			}
			versionRow, err := content.PrepareVersion(t.Context(), org, versionRaw)
			if err != nil {
				t.Fatal(err)
			}
			projectPolicy, err := content.PrepareProjectPolicy(t.Context(), org, hubTestPolicy())
			if err != nil {
				t.Fatal(err)
			}
			projectPolicy.ProjectID, projectPolicy.ApprovedBy, projectPolicy.ApprovedAt = "project", "owner", now
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", Slug: string(org), CreatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: string(org)}); err != nil {
					return err
				}
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project", Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "check", Name: "Example check", Hash: apikey.HashToken(token), Scope: "operator", ExpiresAt: &expires, Kind: "service", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "check", ProjectID: "project"}); err != nil {
					return err
				}
				if err := w.ApproveProjectPolicy(t.Context(), projectPolicy, ""); err != nil {
					return err
				}
				if err := w.SetReviewPolicy(t.Context(), row, ""); err != nil {
					return err
				}
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "project", NativeID: "item", Number: 1, Title: "Example"}); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), issueRow); err != nil {
					return err
				}
				if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
					return err
				}
				if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
					return err
				}
				return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: "project", WorkItemID: "item", ChangeID: "change", CreatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			e := echo.New()
			e.Use(middleware.Recover())
			service.registerChangeRoutes(e)
			var firstResponse string
			send := func(path, token string, want int) {
				t.Helper()
				response := httptest.NewRecorder()
				e.ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path, token, request))
				requireNativeStatus(t, response, want)
				if want == http.StatusOK {
					if firstResponse != "" && response.Body.String() != firstResponse {
						t.Fatal("receipt replay changed response")
					}
					firstResponse = response.Body.String()
					var check tracker.ChangeCheck
					decodeHubResponse(t, response, &check)
					if check.VersionID != "version" || check.Actor.PrincipalID != "check" || check.Conclusion != "success" {
						t.Fatalf("check=%+v", check)
					}
				}
			}
			send(path, token, http.StatusOK)
			send(path, token, http.StatusOK)
			send(path, "other_token", http.StatusUnauthorized)
			send(path[:len(path)-len("checks")]+"reviews", token, http.StatusNotFound)
			send(strings.Replace(path, string(org), "org_other", 1), token, http.StatusUnauthorized)
			send(strings.Replace(path, "/projects/project/", "/projects/other/", 1), token, http.StatusNotFound)
			now = now.Add(2 * time.Hour)
			send(path, token, http.StatusUnauthorized)
			now = now.Add(-2 * time.Hour)
			for i, source := range []string{"customer", "independent"} {
				changed := rules
				changed.ID = "changed-" + source
				changed.RequiredChecks = []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: "other", Source: source}}
				if i == 0 {
					changed.RequiredChecks[0].PrincipalID = "check"
				}
				changedRow := row
				changedRow.Revision = int64(i + 2)
				changedRow, err = content.PrepareReviewPolicy(t.Context(), changedRow, changed)
				if err != nil {
					t.Fatal(err)
				}
				service.config.TenantRepository = &projectUpdateBeforeWriteRepository{Repository: repository, before: func() error {
					return repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.SetReviewPolicy(t.Context(), changedRow, rules.ID) })
				}}
				send(path, token, http.StatusNotFound)
				service.config.TenantRepository = repository
				send(path, token, http.StatusNotFound)
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.SetReviewPolicy(t.Context(), row, changed.ID) }); err != nil {
					t.Fatal(err)
				}
				send(path, token, http.StatusOK)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteAccessTokenGrant(t.Context(), "check", "project") }); err != nil {
				t.Fatal(err)
			}
			send(path, token, http.StatusNotFound)
		})
	}
}
