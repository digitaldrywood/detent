package accessdirectory

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessmembers"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/platformusers"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type directoryFixture struct {
	repository tenantstore.Repository
	now        func() time.Time
	service    *Service
}

func newDirectoryFixture(t *testing.T) directoryFixture {
	repository := hubtest.Repository(t)
	hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: "org_security"})
	f := directoryFixture{repository: repository, now: time.Now}
	f.service = New(Deps{Repository: repository, Organization: "org_security", Now: f.now, Claims: func(echo.Context) (cloudassert.Claims, bool) {
		return cloudassert.Claims{Kind: cloudassert.KindService}, true
	}, Error: hubtest.HTTPError, Invalid: func(c echo.Context, err error) error { return c.NoContent(http.StatusUnprocessableEntity) }})
	return f
}
func (f directoryFixture) member(t *testing.T, name, role, grant string, subjects ...string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	subject := "user_" + name
	if len(subjects) > 0 {
		subject = subjects[0]
	}
	identity := auth.Identity{Subject: subject, Email: name + "@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: subject, OrganizationID: "provider_org_security", SessionID: "session_" + name, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
	membership := auth.Membership{ID: "membership_" + subject, UserID: subject, OrganizationID: "provider_org_security", Status: "active"}
	membership.Role.Slug = role
	members := accessmembers.New(accessmembers.Deps{Repository: f.repository, Organization: "org_security", Now: f.now, GenerateToken: apikey.GenerateToken})
	if err := members.Publish(t.Context(), identity, membership, ""); err != nil {
		t.Fatal(err)
	}
}
func (f directoryFixture) serve(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/platform/users", strings.NewReader(body)).WithContext(t.Context())
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	response := httptest.NewRecorder()
	if err := f.service.ServeHTTP(echo.New().NewContext(request, response)); err != nil {
		t.Fatal(err)
	}
	return response
}
func TestHostedPlatformUsers(t *testing.T) {
	t.Parallel()
	f := newDirectoryFixture(t)
	f.member(t, "match", "owner", "")
	f.member(t, "match-extra", "viewer", "")
	f.member(t, "inactive-match", "member", "")
	if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), "user_inactive-match")
		if err != nil {
			return err
		}
		member.Active, member.UpdatedAt = false, f.now()
		return w.UpdateAccessMember(t.Context(), member)
	}); err != nil {
		t.Fatal(err)
	}
	now := f.now()
	for _, item := range []struct {
		id, email, accepted string
		expires             time.Time
	}{
		{"pending", "match@example.test", "", now.Add(time.Hour)},
		{"invitation-only", "match-invited@example.test", "", now.Add(time.Hour)},
		{"expired", "match-expired@example.test", "", now.Add(-time.Hour)},
		{"accepted", "match-accepted@example.test", "user_accepted", now.Add(time.Hour)},
	} {
		if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessInvitation(t.Context(), accessstore.Invitation{OrganizationID: "org_security", ID: item.id, Email: item.email, Role: "member", AcceptedUserID: item.accepted, CreatedAt: now, ExpiresAt: item.expires})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 55 {
		email := fmt.Sprintf("cap%02d@example.test", i)
		if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessInvitation(t.Context(), accessstore.Invitation{OrganizationID: "org_security", ID: email, Email: email, Role: "member", CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 55 {
		f.member(t, fmt.Sprintf("large%02d", i), "member", "", fmt.Sprintf("user_large%02d%s", i, strings.Repeat("s", 2000)))
	}
	for _, test := range []struct {
		query                        string
		status, members, invitations int
	}{
		{"MATCH@example.test", 200, 1, 1},
		{"match", 200, 2, 2},
		{"match@example.test.example.org", 200, 0, 0},
		{"cap", 200, 0, 55},
		{"missing", 200, 0, 0},
		{"ab", 200, 0, 0},
		{"", 200, 57, 57},
		{"large", 200, 55, 0},
	} {
		t.Run(test.query, func(t *testing.T) {
			result := platformusers.TenantResult{}
			cursor := ""
			seen := map[string]bool{}
			for {
				input := map[string]string{"q": test.query}
				if cursor != "" {
					input["after"] = cursor
				}
				body, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				response := f.serve(t, string(body))
				if response.Code != test.status || response.Body.Len() > 64<<10 {
					t.Fatalf("search = %d, %d bytes", response.Code, response.Body.Len())
				}
				var page platformusers.TenantResult
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if len(page.Members)+len(page.Invitations) > 50 || strings.Contains(response.Body.String(), "private-project-sentinel") {
					t.Fatal("unbounded page or customer content leak")
				}
				result.Members = append(result.Members, page.Members...)
				result.Invitations = append(result.Invitations, page.Invitations...)
				cursor = page.NextCursor
				if cursor == "" {
					break
				}
				if seen[cursor] {
					t.Fatal("cursor did not advance")
				}
				seen[cursor] = true
			}
			if len(result.Members) != test.members || len(result.Invitations) != test.invitations {
				t.Fatalf("result totals = %d members, %d invitations", len(result.Members), len(result.Invitations))
			}
			for _, member := range result.Members {
				if member.JoinedAt == "" || member.Subject == "" || member.Role == "" {
					t.Fatalf("member = %+v", member)
				}
			}
		})
	}
}
