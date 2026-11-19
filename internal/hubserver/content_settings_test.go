package hubserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCloudContentSettingsAdministration(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	var revision tracker.Revision
	for _, role := range []string{"viewer", "member", "reporter", "owner", "admin"} {
		t.Run(role, func(t *testing.T) {
			user := f.user(t, role, role, role+"@example.test", "write", "")
			request := cloudContentSettingsRequest{Mutation: tracker.Mutation{IdempotencyKey: role}, ContentChange: projectsettings.ContentChange{ExpectedRevision: revision, RequireVisualAttachments: true}}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			response := f.serve(t, hostedSharedRequest{user: &user, method: http.MethodPut, target: f.base + "/content-settings", body: string(raw), csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")})
			if role == "owner" || role == "admin" {
				requireNativeStatus(t, response, 200)
				revision++
			} else if response.Code != 401 && response.Code != 403 && response.Code != 404 {
				t.Fatalf("non-administrator status=%d %s", response.Code, response.Body.String())
			}
		})
	}
}
