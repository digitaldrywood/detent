package operatortool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

// Discovery, approval and annotations must not retain permission after the
// application's current authority changes, or reach a read on denial.
func TestCurrentAuthorityExecution(t *testing.T) {
	for _, test := range []struct {
		name                string
		change              func(*Connection, *Authority)
		denied              bool
		message             string
		providerUnavailable bool
	}{
		{name: "current grant"},
		{name: "provider unavailable resolving authority", denied: true, providerUnavailable: true, change: func(c *Connection, a *Authority) {
			c.Resolve = func(context.Context) (Authority, error) {
				return Authority{}, fmt.Errorf("private provider detail: %w", ErrProviderUnavailable)
			}
		}},
		{name: "provider unavailable checking authority", denied: true, providerUnavailable: true, change: func(c *Connection, a *Authority) {
			a.Check = func(context.Context, Requirement) error {
				return fmt.Errorf("private provider detail: %w", ErrProviderUnavailable)
			}
		}},
		{name: "revoked credential", denied: true, change: func(c *Connection, a *Authority) {
			c.Resolve = func(context.Context) (Authority, error) { return Authority{}, errors.New("secret lookup detail") }
		}},
		{name: "removed membership or downgraded grant", denied: true, change: func(c *Connection, a *Authority) {
			a.Check = func(context.Context, Requirement) error { return errors.New("secret project") }
		}},
		{name: "named project denial", denied: true, message: "project missing", change: func(c *Connection, a *Authority) {
			a.Check = func(context.Context, Requirement) error { return fmt.Errorf("%w: project missing", ErrAccessDenied) }
		}},
		{name: "forged organization", denied: true, change: func(c *Connection, a *Authority) { a.Identity.OrganizationID = "other" }},
		{name: "changed principal", denied: true, change: func(c *Connection, a *Authority) { a.Identity.PrincipalID = "other" }},
		{name: "changed session", denied: true, change: func(c *Connection, a *Authority) { a.Identity.SessionID = "other" }},
		{name: "changed credential", denied: true, change: func(c *Connection, a *Authority) { a.Identity.CredentialID = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity := Identity{PrincipalID: "member", OrganizationID: "org", CredentialID: "key", SessionID: "session"}
			authority := Authority{Identity: identity, Check: func(context.Context, Requirement) error { return nil }, Snapshot: func(ctx context.Context, snapshot telemetry.Snapshot) (telemetry.Snapshot, error) {
				return snapshot, nil
			}}
			connection := Connection{Identity: identity, Resolve: func(context.Context) (Authority, error) { return authority, nil }}
			reads := 0
			executor := NewAuthorizedExecutor(NewExecutor(Dependencies{Snapshots: SnapshotFunc(func(context.Context) (telemetry.Snapshot, error) {
				reads++
				return telemetry.Snapshot{GeneratedAt: time.Now()}, nil
			})}))
			ctx := WithConnection(t.Context(), connection)
			if _, err := executor.ListTools(ctx); err != nil {
				t.Fatal(err)
			}
			if test.change != nil {
				test.change(&connection, &authority)
			}
			ctx = WithConnection(t.Context(), connection)
			_, err := executor.Execute(ctx, Call{Name: BoardState, Arguments: json.RawMessage(`{"project_id":"project"}`)})
			if test.denied {
				wantError := ErrAccessDenied
				if test.providerUnavailable {
					wantError = ErrProviderUnavailable
				}
				if !errors.Is(err, wantError) || reads != 0 {
					t.Fatalf("err=%v reads=%d", err, reads)
				}
				if test.message != "" && !strings.Contains(err.Error(), test.message) {
					t.Fatalf("denial detail lost: %v", err)
				}
				// An approved action must resolve again, exactly like a direct call.
				if _, err := AuthorizeCurrent(ctx, Requirement{Scope: apikey.ScopeWrite, ProjectID: "project"}); !errors.Is(err, wantError) {
					t.Fatalf("approved action authorization = %v", err)
				}
			} else if err != nil || reads != 1 {
				t.Fatalf("err=%v reads=%d", err, reads)
			}
		})
	}
	if _, err := AuthorizeCurrent(t.Context(), Requirement{Scope: apikey.ScopeRead}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("missing connection: %v", err)
	}
}
