package hubserver

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) {
	flag.Parse()
	timeout, err := time.ParseDuration(flag.Lookup("test.timeout").Value.String())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := postgresStartupTemplate.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	cancel()
	os.Exit(code)
}

const defaultHubOperatorID = "tok_test_default"

func nativeFixtureStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
}

func newDefaultNativeFixture(t *testing.T, cfg Config) nativeFixture {
	t.Helper()
	return newNativeFixture(t, newClockedPostgresNativeService(t, cfg), "", "Example project")
}

func TestNativeDatabaseTemplateIsolation(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"first copy", "second copy"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t, Config{})
			issue := f.create(t, "template-isolation")
			if issue.Number != 1 {
				t.Fatalf("fresh fixture issue number = %d, want 1", issue.Number)
			}
			credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.RevokeAccessCredential(t.Context(), credential.ID, f.service.config.now())
			}); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base, f.token, nil), http.StatusUnauthorized)
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "additional")
			if other.project.ID == f.project.ID || other.token == f.token {
				t.Fatal("named fixture reused the default project or token")
			}
			other.create(t, "template-isolation")
		})
	}
}
