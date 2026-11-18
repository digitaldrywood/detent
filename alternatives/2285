package nativework_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestNativeDatabaseTemplateIsolation(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"first copy", "second copy"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
			issue := f.Create(t, "template-isolation")
			if issue.Number != 1 {
				t.Fatalf("fresh fixture issue number = %d, want 1", issue.Number)
			}
			credential, err := f.Credential(t, f.Token)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Hub.Config.TenantRepository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.RevokeAccessCredential(t.Context(), credential.ID, f.Hub.Config.Clock())
			}); err != nil {
				t.Fatal(err)
			}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodGet, f.Base, f.Token, nil), http.StatusUnauthorized)
			other := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "additional")
			if other.Project.ID == f.Project.ID || other.Token == f.Token {
				t.Fatal("named fixture reused the default project or token")
			}
			other.Create(t, "template-isolation")
		})
	}
}
