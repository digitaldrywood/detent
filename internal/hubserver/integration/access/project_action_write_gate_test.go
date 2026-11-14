package access_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
)

func TestProjectActionWriteGateRefusesAViewerAndAReadOnlyGrant(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		role  string
		grant string
		read  int
		write int
	}{
		{name: "a member with write may author", role: "member", grant: "write", read: http.StatusOK, write: http.StatusCreated},
		{name: "a viewer may read and never write", role: "viewer", grant: "write", read: http.StatusOK, write: http.StatusForbidden},
		{name: "a read-only grant may read and never write", role: "member", grant: "read", read: http.StatusOK, write: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := hubfixture.NewHostedSecurity(t)
			user := f.User(t, test.role, test.role, test.role+"@example.test", test.grant, "")
			hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, f.Base+"/actions", nil), test.read)
			hubfixture.RequireStatus(t, f.Request(t, user, http.MethodPost, f.Base+"/actions", map[string]any{
				"idempotency_key": "author-one", "name": "Run tests", "command": "go test ./...",
			}), test.write)
		})
	}
}
