package hubserver

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func TestEntryKeyCurrentAuthority(t *testing.T) {
	for _, kind := range []string{"personal", "service"} {
		for _, access := range []string{"all", "selected", "project"} {
			for _, role := range []string{"owner", "admin", "member", "viewer"} {
				for _, member := range []bool{true, false} {
					name := fmt.Sprintf("%s/%s/%s/member=%v", kind, access, role, member)
					t.Run(name, func(t *testing.T) {
						f := newHostedSharedFixture(t)
						user := f.user(t, "key_owner", role, "key-owner@example.test", "write", "")
						if !member {
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET active=0 WHERE user_id=?", user.identity.Subject)
						}
						projects := []string(nil)
						if access != "all" {
							projects = []string{string(f.project)}
						}
						key := apikey.KeyAuthority{ID: "key_matrix", Kind: kind, Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: access, Projects: projects}}
						mutate := func(claims *cloudassert.Claims) {
							claims.Key, claims.Subject, claims.ProviderOrganization, claims.Role = &key, user.identity.Subject, "org_provider", role
							if kind == "service" {
								claims.Subject, claims.Role = "service:"+key.ID, "admin"
							}
						}
						read := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate})
						wantRead := http.StatusOK
						if kind == "personal" && !member {
							wantRead = http.StatusForbidden
						}
						if read.Code != wantRead {
							t.Fatalf("read=%d want=%d %s", read.Code, wantRead, read.Body.String())
						}
						write := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: f.base + "/work-items", body: `{"idempotency_key":"matrix-write","title":"Scope and membership regression","state":"Todo"}`, mutate: mutate})
						wantWrite := http.StatusOK
						if kind == "personal" && !member {
							wantWrite = http.StatusForbidden
						} else if kind == "personal" && role == "viewer" {
							wantWrite = http.StatusNotFound
						}
						if write.Code != wantWrite {
							t.Fatalf("write=%d want=%d %s", write.Code, wantWrite, write.Body.String())
						}
						if kind == "service" || member {
							claims := f.claims(nil, cloudassert.KindKey, http.MethodGet, f.base+"/work-items", nil)
							mutate(&claims)
							credential, _, err := f.service.entryKeyCredential(t.Context(), claims)
							if err != nil {
								t.Fatal(err)
							}
							visible, err := f.service.hostedReadableProjects(t.Context(), credential)
							if err != nil || len(visible) != 1 || visible[0].ID != string(f.project) {
								t.Fatalf("live key projects=%v error=%v", visible, err)
							}
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET deleted_at='2026-10-09T00:00:00Z' WHERE id=?", f.project)
							denied := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate})
							if denied.Code != http.StatusNotFound {
								t.Fatalf("deleted project read=%d %s", denied.Code, denied.Body.String())
							}
							visible, err = f.service.hostedReadableProjects(t.Context(), credential)
							if err != nil || len(visible) != 0 {
								t.Fatalf("deleted key projects=%v error=%v", visible, err)
							}
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET deleted_at=NULL WHERE id=?", f.project)
						}
						if kind == "personal" && member {
							operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", user.identity.Subject)
							denied := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate})
							if denied.Code != http.StatusNotFound {
								t.Fatalf("removed grant=%d %s", denied.Code, denied.Body.String())
							}
						}
						if access != "all" {
							key.Projects = []string{"prj_other"}
							denied := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate})
							if kind == "service" || member {
								if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "key_project_denied") {
									t.Fatalf("scope refusal=%d %s", denied.Code, denied.Body.String())
								}
							}
						}
					})
				}
			}
		}
	}
}
