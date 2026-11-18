package operatorchat_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorchat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func TestPostgresOperatorChatSessions(t *testing.T) {
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin)
	now := time.Now().UTC().Truncate(time.Microsecond)
	var organizations []string
	for range 2 {
		organizations = append(organizations, string(hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: now}).OrganizationID))
	}
	store := operatorchat.Store{Repository: repository}
	for _, org := range organizations {
		identity := operatortool.Identity{OrganizationID: org, PrincipalID: "admin", CredentialID: apikey.HashToken(org)}
		ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity})
		if err := store.Save(ctx, chat.SessionState{ID: "connection", Identity: identity, Client: "example", LastUsedAt: now}, now, time.Hour, 128); err != nil {
			t.Fatal(err)
		}
		state := chat.SessionState{ID: "shared", Identity: identity, Client: "example", LastUsedAt: now, Origin: `{"key_id":"example-key","credential_binding":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","subject":"admin","issuer":"entry","generation":1}`}
		if err := store.Save(ctx, state, now, time.Hour, 128); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			at    time.Time
			found bool
		}{{now, true}, {now.Add(time.Hour), true}, {now.Add(time.Hour + time.Microsecond), false}} {
			got, found, err := store.Load(ctx, state.ID, test.at, time.Hour)
			if err != nil || found != test.found || found && (got.Identity != identity || got.Client != "example" || got.Origin != state.Origin) {
				t.Fatalf("load=%+v found=%v err=%v", got, found, err)
			}
		}
		restoredContext := context.WithValue(t.Context(), operatorchat.OrganizationKey{}, tracker.OrganizationID(org))
		if restored, found, err := store.Load(restoredContext, state.ID, now, time.Hour); err != nil || !found || restored.Identity != identity {
			t.Fatalf("coordinator restore=%+v %v %v", restored, found, err)
		}
		changed := state
		changed.Identity.PrincipalID = "different"
		if err := store.Save(ctx, changed, now, time.Hour, 128); !errors.Is(err, operatortool.ErrAccessDenied) {
			t.Fatalf("identity overwrite=%v", err)
		}
		restored, found, err := store.Load(ctx, state.ID, now, time.Hour)
		if err != nil || !found || restored.Identity != identity {
			t.Fatalf("identity after refusal=%+v %v %v", restored, found, err)
		}
		state.ID = "newest"
		state.LastUsedAt = now.Add(time.Second)
		if err := store.Save(ctx, state, now, time.Hour, 2); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM operator_chat_sessions WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 2 {
			t.Fatalf("bounded sessions=%d err=%v", count, err)
		}
		if _, found, err := store.Load(ctx, "newest", now, time.Hour); err != nil || !found {
			t.Fatalf("new session missing: %v", err)
		}
	}
}
