package platformstore

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSessionsAndTenantAuthorizations(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	sessions := []Session{createSession(t, r, "first"), createSession(t, r, "second")}
	for _, s := range sessions {
		for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
			t.Run(s.Subject+"/"+string(organization), func(t *testing.T) {
				a := authorize(t, r, s, organization, "provider_session_"+s.Subject+"_"+string(organization))
				got, err := r.Session(t.Context(), s.Hash)
				if err != nil || got.Subject != s.Subject || got.CSRFSecret != s.CSRFSecret {
					t.Fatalf("session: %+v %v", got, err)
				}
				stored, err := r.Authorization(t.Context(), s.Hash, organization)
				if err != nil || stored.Binding != a.Binding || stored.OrganizationID != organization || !sameIdentity(stored.Identity, a.Identity) {
					t.Fatalf("authorization: %+v %v", stored, err)
				}
				tokens, err := r.Tokens(t.Context(), s.Hash, organization, a.Binding)
				if err != nil || tokens.AccessToken != "access_"+a.Identity.SessionID {
					t.Fatalf("tokens: %+v %v", tokens, err)
				}
				other := tracker.OrganizationID("org_first")
				if organization == other {
					other = "org_second"
				}
				if _, err := r.Tokens(t.Context(), s.Hash, other, a.Binding); !errors.Is(err, ErrNoSession) {
					t.Fatalf("wrong organization read: %v", err)
				}
				if _, err := r.Tokens(t.Context(), "other_session", organization, a.Binding); !errors.Is(err, ErrNoSession) {
					t.Fatalf("wrong session read: %v", err)
				}
				if err := r.RevokeAuthorization(t.Context(), s.Hash, other, a.Binding); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Tokens(t.Context(), s.Hash, organization, a.Binding); err != nil {
					t.Fatalf("cross-tenant revoke affected original: %v", err)
				}
			})
		}
	}
	first := sessions[0]
	for _, tc := range []struct {
		name     string
		subject  string
		provider string
		actor    string
		want     error
	}{
		{"wrong user", "another_subject", "provider_first", "", ErrNoSession},
		{"wrong provider tenant", first.Subject, "provider_second", "", ErrNoSession},
		{"wrong support actor", "another_subject", "provider_first", "other@example.com", ErrNoSession},
		{"support session", "another_subject", "provider_first", first.Email, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := first.Identity
			identity.Subject = tc.subject
			identity.OrganizationID = tc.provider
			identity.SupportActor = tc.actor
			identity.SessionID = tc.name
			a, _, err := r.Authorize(t.Context(), first.Hash, "org_first", identity, auth.HostedTokens{}, first.Email)
			if !errors.Is(err, tc.want) {
				t.Fatalf("authorize: %v", err)
			}
			if err == nil {
				stored, err := r.Authorization(t.Context(), first.Hash, "org_first")
				if err != nil || !stored.Support || stored.Binding != a.Binding {
					t.Fatalf("support: %+v %v", stored, err)
				}
			}
		})
	}
	if err := r.RevokeSession(t.Context(), first.Hash); err != nil {
		t.Fatal(err)
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := r.Authorization(t.Context(), first.Hash, organization); !errors.Is(err, ErrNoSession) {
			t.Fatalf("revoked session authorization: %v", err)
		}
		if _, err := r.Authorization(t.Context(), sessions[1].Hash, organization); err != nil {
			t.Fatalf("unrelated session affected: %v", err)
		}
	}
	if _, err := r.Session(t.Context(), first.Hash); !errors.Is(err, ErrNoSession) {
		t.Fatal(err)
	}
}

func TestExpiredSessionsAndAuthorizations(t *testing.T) {
	for _, target := range []string{"session", "authorization"} {
		t.Run(target, func(t *testing.T) {
			db := template.Database(t)
			r := repository(t, db, testConfig(t))
			seedOrganizations(t, r)
			s := createSession(t, r, "first")
			a := authorize(t, r, s, "org_first", "provider_session")
			query := `UPDATE platform.sessions SET expires_at=$1 WHERE token_hash=$2`
			id := s.Hash
			if target == "authorization" {
				query = `UPDATE platform.authorizations SET expires_at=$1 WHERE binding=$2`
				id = a.Binding
			}
			if _, err := db.ExecContext(t.Context(), query, r.now(), id); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Authorization(t.Context(), s.Hash, "org_first"); !errors.Is(err, ErrNoSession) {
				t.Fatalf("boundary authorization: %v", err)
			}
			if _, err := r.Tokens(t.Context(), s.Hash, "org_first", a.Binding); !errors.Is(err, ErrNoSession) {
				t.Fatalf("boundary tokens: %v", err)
			}
			if target == "session" {
				stored, err := r.StoredSession(t.Context(), s.Hash)
				if err != nil || stored.Hash != s.Hash || stored.ExpiresAt.After(r.now()) {
					t.Fatalf("expired stored session=%+v err=%v", stored, err)
				}
				if _, err := r.Session(t.Context(), s.Hash); !errors.Is(err, ErrNoSession) {
					t.Fatal(err)
				}
				identity := s.Identity
				identity.OrganizationID = "provider_first"
				if _, _, err := r.Authorize(t.Context(), s.Hash, "org_first", identity, auth.HostedTokens{}, s.Email); !errors.Is(err, ErrNoSession) {
					t.Fatalf("expired session authorized: %v", err)
				}
			}
		})
	}
}

func TestConcurrentSessionMutation(t *testing.T) {
	for _, operation := range []string{"replace authorization", "refresh tokens", "revoke session", "revoke authorization"} {
		t.Run(operation, func(t *testing.T) {
			db := template.Database(t)
			cfg := testConfig(t)
			first := repository(t, db, cfg)
			second := replicaRepository(t, first, cfg)
			seedOrganizations(t, first)
			s := createSession(t, first, "first")
			a := authorize(t, first, s, "org_first", "initial")
			unrelated := createSession(t, first, "second")
			authorize(t, first, unrelated, "org_first", "unrelated")
			previous := auth.HostedTokens{AccessToken: "access_initial", RefreshToken: "refresh_initial"}
			repos := []*Repository{first, second}
			start := make(chan struct{})
			results := make(chan error, len(repos))
			var workers sync.WaitGroup
			for index, r := range repos {
				workers.Go(func() {
					<-start
					switch operation {
					case "replace authorization":
						identity := s.Identity
						identity.OrganizationID = "provider_first"
						identity.SessionID = fmt.Sprintf("replacement_%d", index)
						_, _, err := r.Authorize(t.Context(), s.Hash, "org_first", identity, auth.HostedTokens{}, s.Email)
						results <- err
					case "refresh tokens":
						results <- r.RotateTokens(t.Context(), s.Hash, "org_first", a.Binding, previous, auth.HostedTokens{AccessToken: fmt.Sprintf("access_%d", index), RefreshToken: fmt.Sprintf("refresh_%d", index)})
					case "revoke session":
						if index == 0 {
							results <- r.RevokeSession(t.Context(), s.Hash)
						} else {
							identity := s.Identity
							identity.OrganizationID = "provider_first"
							identity.SessionID = "racing_replacement"
							_, _, err := r.Authorize(t.Context(), s.Hash, "org_first", identity, auth.HostedTokens{}, s.Email)
							results <- err
						}
					case "revoke authorization":
						if index == 0 {
							results <- r.RevokeAuthorization(t.Context(), s.Hash, "org_first", a.Binding)
						} else {
							results <- r.RotateTokens(t.Context(), s.Hash, "org_first", a.Binding, previous, auth.HostedTokens{AccessToken: "updated", RefreshToken: "updated"})
						}
					}
				})
			}
			close(start)
			workers.Wait()
			close(results)
			successes, conflicts := 0, 0
			for err := range results {
				switch {
				case err == nil:
					successes++
				case operation == "refresh tokens" && errors.Is(err, corestore.ErrRevisionConflict):
					conflicts++
				case (operation == "revoke session" || operation == "revoke authorization") && errors.Is(err, ErrNoSession):
				default:
					t.Fatal(err)
				}
			}
			switch operation {
			case "replace authorization":
				if successes != 2 {
					t.Fatalf("successful replacements=%d", successes)
				}
				var active int
				if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM platform.authorizations WHERE organization_id='org_first' AND session_hash=$1 AND revoked_at IS NULL`, s.Hash).Scan(&active); err != nil || active != 1 {
					t.Fatalf("active=%d err=%v", active, err)
				}
				if _, err := first.Authorization(t.Context(), s.Hash, "org_first"); err != nil {
					t.Fatal(err)
				}
			case "refresh tokens":
				if successes != 1 || conflicts != 1 {
					t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
				}
				tokens, err := first.Tokens(t.Context(), s.Hash, "org_first", a.Binding)
				if err != nil || tokens == previous {
					t.Fatalf("tokens unchanged: %+v %v", tokens, err)
				}
			case "revoke session", "revoke authorization":
				if _, err := first.Authorization(t.Context(), s.Hash, "org_first"); !errors.Is(err, ErrNoSession) {
					t.Fatalf("authorization survived revocation: %v", err)
				}
				if _, err := first.Tokens(t.Context(), s.Hash, "org_first", a.Binding); !errors.Is(err, ErrNoSession) {
					t.Fatalf("tokens survived revocation: %v", err)
				}
				if operation == "revoke session" {
					identity := s.Identity
					identity.OrganizationID = "provider_first"
					if _, _, err := first.Authorize(t.Context(), s.Hash, "org_first", identity, auth.HostedTokens{}, s.Email); !errors.Is(err, ErrNoSession) {
						t.Fatalf("revoked session resurrected: %v", err)
					}
				}
			}
			if _, err := first.Authorization(t.Context(), unrelated.Hash, "org_first"); err != nil {
				t.Fatalf("unrelated session: %v", err)
			}
		})
	}
}

func TestRevokedBindingCannotBeResurrected(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	s := createSession(t, r, "first")
	a := authorize(t, r, s, "org_first", "initial")
	if err := r.RevokeAuthorization(t.Context(), s.Hash, "org_first", a.Binding); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Authorize(t.Context(), s.Hash, "org_first", a.Identity, auth.HostedTokens{}, s.Email); !errors.Is(err, ErrNoSession) {
		t.Fatalf("binding resurrected: %v", err)
	}
	authorize(t, r, s, "org_first", "new_provider_session")
}

func sameIdentity(a, b auth.HostedIdentity) bool {
	return a.Subject == b.Subject && a.OrganizationID == b.OrganizationID && a.SessionID == b.SessionID && a.CreatedAt.Equal(b.CreatedAt) && a.ExpiresAt.Equal(b.ExpiresAt) && a.SupportActor == b.SupportActor && a.SupportReason == b.SupportReason
}

func TestOrganizationMappingChangesDenyAuthorization(t *testing.T) {
	for _, mutation := range []struct{ name, query string }{
		{"disabled", `UPDATE platform.organizations SET state='disabled' WHERE id='org_first'`},
		{"provider mapping changed", `UPDATE platform.organizations SET provider_id='provider_other' WHERE id='org_first'`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			db := template.Database(t)
			r := repository(t, db, testConfig(t))
			seedOrganizations(t, r)
			s := createSession(t, r, "first")
			a := authorize(t, r, s, "org_first", "provider_session")
			authorize(t, r, s, "org_second", "other")
			if mutation.name == "provider mapping changed" {
				if _, err := db.ExecContext(t.Context(), mutation.query); err == nil {
					t.Fatal("provider identity mutation bypassed the registry fence")
				}
				tx, err := db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err := tx.ExecContext(t.Context(), "SET LOCAL session_replication_role = replica"); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), mutation.query); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.ExecContext(t.Context(), mutation.query); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.Authorization(t.Context(), s.Hash, "org_first"); !errors.Is(err, ErrNoSession) {
				t.Fatalf("changed tenant authorized: %v", err)
			}
			if _, err := r.Tokens(t.Context(), s.Hash, "org_first", a.Binding); !errors.Is(err, ErrNoSession) {
				t.Fatalf("changed tenant tokens: %v", err)
			}
			if _, err := r.Authorization(t.Context(), s.Hash, "org_second"); err != nil {
				t.Fatalf("unrelated tenant: %v", err)
			}
		})
	}
}

func TestRevocationFencesAnInFlightAuthorization(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	s := createSession(t, r, "first")
	db.SetMaxOpenConns(2)
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE platform.sessions SET revoked_at=$1 WHERE token_hash=$2`, r.now(), s.Hash); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	results := make(chan error, 1)
	var workers sync.WaitGroup
	t.Cleanup(func() { tx.Rollback(); workers.Wait() })
	workers.Go(func() {
		identity := s.Identity
		identity.OrganizationID = "provider_first"
		_, _, err := r.Authorize(t.Context(), s.Hash, "org_first", identity, auth.HostedTokens{}, s.Email)
		results <- err
	})
	completed := false
	var completedErr error
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case completedErr = <-results:
			completed = true
			return nil
		default:
		}
		var waiting bool
		err := db.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%platform.sessions%FOR UPDATE%')`).Scan(&waiting)
		if err != nil {
			return err
		}
		if waiting {
			return nil
		}
		return errors.New("authorization has not reached the session fence")
	})
	if completed {
		t.Fatalf("authorization passed an uncommitted revocation: %v", completedErr)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-results; !errors.Is(err, ErrNoSession) {
		t.Fatalf("authorization survived committed revocation: %v", err)
	}
	workers.Wait()
}
