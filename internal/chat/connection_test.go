package chat

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func connectionTestAction() Action {
	return Action{Kind: ActionStopRun, ProjectID: "project", RequestID: "request", Arguments: json.RawMessage(`{"destination":"Cancelled"}`), Labels: []string{"original"}}
}

func TestConnectionActions(t *testing.T) {
	for _, scenario := range []struct {
		name                            string
		scope                           apikey.Scope
		revoked, expired, foreign, idle bool
		restored                        bool
		evicted                         bool
		denied                          bool
	}{
		{name: "write", scope: apikey.ScopeWrite}, {name: "admin", scope: apikey.ScopeAdmin},
		{name: "read", scope: apikey.ScopeRead, denied: true}, {name: "revoked", scope: apikey.ScopeWrite, revoked: true, denied: true},
		{name: "expired", scope: apikey.ScopeWrite, expired: true, denied: true}, {name: "foreign project", scope: apikey.ScopeWrite, foreign: true, denied: true},
		{name: "idle", scope: apikey.ScopeWrite, idle: true},
		{name: "restored identity", scope: apikey.ScopeWrite, restored: true},
		{name: "evicted during attachment", scope: apikey.ScopeWrite, evicted: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			executor := &actionExecutorStub{result: "completed"}
			service := newTestService(nil, nil, executor)
			identity := operatortool.Identity{PrincipalID: "operator", OrganizationID: "organization", CredentialID: "credential"}
			revoked := false
			ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{ID: "connection", Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
				if revoked || scenario.revoked || scenario.expired {
					return operatortool.Authority{}, operatortool.ErrAccessDenied
				}
				return operatortool.Authority{Identity: identity, Check: func(_ context.Context, r operatortool.Requirement) error {
					if scenario.foreign || r.Scope == apikey.ScopeAdmin && scenario.scope != apikey.ScopeAdmin || r.Scope == apikey.ScopeWrite && scenario.scope == apikey.ScopeRead {
						return operatortool.ErrAccessDenied
					}
					return nil
				}}, nil
			}})
			store := &connectionIdentityStore{}
			connection := operatortool.CurrentConnection(ctx)
			resolve := func(ctx context.Context, stored operatortool.Identity) (operatortool.Authority, error) {
				if stored != identity {
					return operatortool.Authority{}, operatortool.ErrAccessDenied
				}
				return connection.Resolve(ctx)
			}
			if scenario.evicted {
				WithSessionStore(store, resolve)(service)
				service.sessionLimit = 1
				store.onSave = func() { service.ensureSession("replacement") }
				if err := service.CheckConnection(ctx); err != nil {
					t.Fatal(err)
				}
				return
			}
			if scenario.restored {
				WithSessionStore(store, resolve)(service)
			}
			if err := service.AttachConnection(ctx); err != nil {
				t.Fatal(err)
			}
			if scenario.restored {
				service = newTestService(nil, nil, executor)
				WithSessionStore(store, resolve)(service)
				if err := service.RestoreConnection(ctx, connection.ID); err != nil {
					t.Fatal(err)
				}
				var err error
				ctx, err = service.OriginatingContext(t.Context(), connection.ID, identity.PrincipalID, identity.OrganizationID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario.idle {
				now := service.now()
				service.now = func() time.Time { return now.Add(25 * time.Hour) }
			}
			result, err := service.Submit(ctx, connectionTestAction())
			if scenario.denied {
				if !errors.Is(err, operatortool.ErrAccessDenied) || executor.calls != 0 {
					t.Fatalf("denied=%+v %v calls=%d", result, err, executor.calls)
				}
				return
			}
			if err != nil || result.Status != ActionSucceeded || executor.calls != 1 {
				t.Fatalf("direct=%+v %v calls=%d", result, err, executor.calls)
			}
			if retry, err := service.Submit(ctx, connectionTestAction()); err != nil || retry.ID != result.ID || executor.calls != 1 {
				t.Fatalf("retry=%+v %v calls=%d", retry, err, executor.calls)
			}
			revoked = true
			if _, err := service.Submit(ctx, connectionTestAction()); !errors.Is(err, operatortool.ErrAccessDenied) || executor.calls != 1 {
				t.Fatalf("revoked replay=%v calls=%d", err, executor.calls)
			}
		})
	}
}

type connectionIdentityStore struct {
	state  SessionState
	onSave func()
}

func (store *connectionIdentityStore) Load(_ context.Context, id string, now time.Time, ttl time.Duration) (SessionState, bool, error) {
	return store.state, store.state.ID == id && now.Sub(store.state.LastUsedAt) <= ttl, nil
}
func (store *connectionIdentityStore) Save(_ context.Context, state SessionState, _ time.Time, _ time.Duration, _ int) error {
	store.state = state
	if store.onSave != nil {
		store.onSave()
	}
	return nil
}
