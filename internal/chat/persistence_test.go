package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type failingSessionStore struct {
	fail bool
}

func (*failingSessionStore) Load(context.Context, string, time.Time, time.Duration) (SessionState, bool, error) {
	return SessionState{}, false, nil
}

func (store *failingSessionStore) Save(context.Context, SessionState, time.Time, time.Duration, int) error {
	if store.fail {
		return ErrUnavailable
	}
	return nil
}

func TestFailedPersistenceDoesNotRenewLifetime(t *testing.T) {
	for _, operation := range []string{"proposal", "confirmation", "mode"} {
		t.Run(operation, func(t *testing.T) {
			identity := operatortool.Identity{PrincipalID: "operator", OrganizationID: "organization", CredentialID: "credential"}
			connection := operatortool.Connection{ID: "connection", Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
				return operatortool.Authority{Identity: identity, Check: func(context.Context, operatortool.Requirement) error { return nil }}, nil
			}}
			ctx := operatortool.WithConnection(t.Context(), connection)
			human := WithOperatorApproval(ctx, identity)
			executor := &actionExecutorStub{result: "completed"}
			s := newTestService(nil, nil, executor)
			store := &failingSessionStore{}
			s.store = store
			if err := s.AttachConnection(ctx); err != nil {
				t.Fatal(err)
			}
			var proposed Action
			if operation == "confirmation" {
				var err error
				proposed, err = s.Submit(ctx, connectionTestAction())
				if err != nil {
					t.Fatal(err)
				}
			}
			current := s.sessions[connection.ID]
			before := current.lastUsedAt
			s.now = func() time.Time { return before.Add(time.Hour) }
			store.fail = true
			var err error
			switch operation {
			case "proposal":
				action := connectionTestAction()
				action.Kind = ActionSetPriority
				_, err = s.Submit(ctx, action)
			case "confirmation":
				_, err = s.Confirm(human, connection.ID, proposed.ID)
			case "mode":
				err = s.SetConnectionMode(human, connection.ID, YOLOMode)
			}
			if !errors.Is(err, ErrUnavailable) || !current.lastUsedAt.Equal(before) || executor.calls != 0 {
				t.Fatalf("failed persistence renewed lifetime or executed effect: error=%v before=%s after=%s calls=%d", err, before, current.lastUsedAt, executor.calls)
			}
		})
	}
}
