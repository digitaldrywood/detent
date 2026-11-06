package hubserver

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestPostgresRunnerSpendReporterVisibility(t *testing.T) {
	f := newRunnerPGFixture(t)
	now := time.Now().UTC()
	for index, role := range []string{"reporter", "member"} {
		id := fmt.Sprintf("actor_%d", index)
		if _, err := f.service.config.Hosted.Provider.CreateMembership(t.Context(), id, "provider_org_startup", role); err != nil {
			t.Fatal(err)
		}
		err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: id, Name: id, Hash: apikey.HashToken(id), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: id, MembershipID: "membership_" + id, KeyScope: "write", ProjectAccess: "all", Kind: "personal"}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: "org_startup", UserID: id, Email: id + "@example.test", MembershipID: "membership_" + id, Role: role, Active: true, PrincipalID: id, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: id, ProjectID: "prj_runner", CanWrite: true}); err != nil {
				return err
			}
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: id, ProjectID: "prj_runner"})
		})
		if err != nil {
			t.Fatal(err)
		}
		statements := []string{
			`UPDATE issues SET actor_principal_id=$1 WHERE organization_id='org_startup' AND native_id=$2`,
			`INSERT INTO runner_attempt_token_usage(organization_id,attempt_id,model,project_id,issue_id,usage_day,observed_at,input_tokens,cached_input_tokens,output_tokens) SELECT organization_id,id,'example-model','prj_runner',issue_id,'2026-11-05','2026-11-05T12:00:00Z',` + fmt.Sprint((index+1)*10) + `,2,3 FROM native_attempts WHERE organization_id='org_startup' AND issue_id=(SELECT id FROM issues WHERE organization_id='org_startup' AND native_id=$2) AND $1<>''`,
			`INSERT INTO runner_issue_tokens SELECT organization_id,project_id,issue_id,model,input_tokens,cached_input_tokens,output_tokens,1 FROM runner_attempt_token_usage WHERE organization_id='org_startup' AND issue_id=(SELECT id FROM issues WHERE organization_id='org_startup' AND native_id=$2) AND $1<>''`,
		}
		for _, statement := range statements {
			if _, err := f.admin.ExecContext(t.Context(), statement, id, f.leases[index].WorkItemID); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, statement := range []string{
		`UPDATE native_attempts SET runner_tokens_recorded=true`,
		`INSERT INTO runner_project_daily_tokens SELECT organization_id,project_id,usage_day,model,sum(input_tokens),sum(cached_input_tokens),sum(output_tokens),count(*) FROM runner_attempt_token_usage GROUP BY organization_id,project_id,usage_day,model`,
	} {
		if _, err := f.admin.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/v2/organizations/org_startup/projects/prj_runner/"
	for _, tc := range []struct {
		name, token, query string
		input, sessions    int64
		quantiles          bool
		status             int
	}{
		{"reporter daily", "actor_0", "runner-spend?day=2026-11-05", 0, 0, false, http.StatusForbidden},
		{"reporter own issue", "actor_0", "runner-spend?issue=" + string(f.leases[0].WorkItemID), 10, 1, false, http.StatusOK},
		{"reporter hidden issue", "actor_0", "runner-spend?issue=" + string(f.leases[1].WorkItemID), 0, 0, false, http.StatusOK},
		{"member daily", "actor_1", "runner-spend?day=2026-11-05", 30, 2, false, http.StatusOK},
		{"member other issue", "actor_1", "runner-spend?issue=" + string(f.leases[0].WorkItemID), 10, 1, false, http.StatusOK},
		{"runner daily", f.tokens[0], "runner-spend?day=2026-11-05", 30, 2, false, http.StatusOK},
		{"reporter quantiles", "actor_0", "runner-model-quantiles?model=example-model&limit=20", 0, 0, true, http.StatusForbidden},
		{"member quantiles", "actor_1", "runner-model-quantiles?model=example-model&limit=20", 20, 2, true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, base+tc.query, tc.token, nil)
			requireNativeStatus(t, response, tc.status)
			if tc.status != http.StatusOK {
				return
			}
			if tc.quantiles {
				var got store.ModelTokenQuantiles
				decodeHubResponse(t, response, &got)
				if got.P90InputTokens != tc.input || got.Sessions != tc.sessions {
					t.Fatalf("quantiles=%+v; want input=%d sessions=%d", got, tc.input, tc.sessions)
				}
			} else {
				var got store.TokenSpend
				decodeHubResponse(t, response, &got)
				if got.InputTokens != tc.input || got.Sessions != tc.sessions {
					t.Fatalf("spend=%+v; want input=%d sessions=%d", got, tc.input, tc.sessions)
				}
			}
		})
	}
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE native_attempts SET runner_tokens_recorded=false WHERE organization_id='org_startup' AND id=$1`, f.data[1].AttemptID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, token, query string
		status             int
		input              int64
	}{
		{"reporter daily remains forbidden", "actor_0", "runner-spend?day=2026-11-05", http.StatusForbidden, 0},
		{"reporter quantiles remain forbidden", "actor_0", "runner-model-quantiles?model=example-model&limit=20", http.StatusForbidden, 0},
		{"hidden accounting does not block own issue", "actor_0", "runner-spend?issue=" + string(f.leases[0].WorkItemID), http.StatusOK, 10},
		{"hidden accounting stays hidden", "actor_0", "runner-spend?issue=" + string(f.leases[1].WorkItemID), http.StatusOK, 0},
		{"member accounting remains required", "actor_1", "runner-spend?day=2026-11-05", http.StatusServiceUnavailable, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, base+tc.query, tc.token, nil)
			requireNativeStatus(t, response, tc.status)
			if tc.status == http.StatusOK {
				var got store.TokenSpend
				decodeHubResponse(t, response, &got)
				if got.InputTokens != tc.input {
					t.Fatalf("spend=%+v; want input=%d", got, tc.input)
				}
			}
		})
	}
}
