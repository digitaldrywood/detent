package cloudentry

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPlatformTransformSQLiteAuthenticationParity(t *testing.T) {
	for _, mode := range []string{"legacy", "retained keyring"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Date(2026, 11, 1, 12, 0, 0, 123456000, time.UTC)
			objects := newSpacesFixture(t, false)
			f := newEntryFixture(t, func(cfg *Config) {
				cfg.now = func() time.Time { return now }
				if mode == "retained keyring" {
					cfg.LookupEnv = entrySecretEnvironment(1, 1, 2)
				}
				storage := objects.config()
				storage.Endpoint = "https://storage.example.test"
				cfg.Attachments, cfg.attachmentTransport = &storage, objects.transport
			})
			s := f.service
			for _, organization := range []string{"org_alpha", "org_beta"} {
				identity := auth.HostedIdentity{Subject: "user_alice", OrganizationID: "porg_" + strings.TrimPrefix(organization, "org_"), SessionID: "session_" + organization, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
				if err := s.auth.createSession(t.Context(), "hash_"+organization, "csrf", auth.Identity{Subject: identity.Subject, Email: "alice@example.test", Hosted: &identity}); err != nil {
					t.Fatal(err)
				}
				session, err := s.auth.session(t.Context(), "hash_"+organization)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.auth.authorize(t.Context(), session, organization, identity, auth.HostedTokens{AccessToken: "access_" + organization, RefreshToken: "refresh_" + organization}, session.Email); err != nil {
					t.Fatal(err)
				}
				if err := s.auth.createTransaction(t.Context(), "login_"+organization, loginTransaction{ID: "transaction_" + organization, State: "state", Verifier: "verifier", InvitationToken: "invitation", InvitationOrganization: organization}); err != nil {
					t.Fatal(err)
				}
				if err := s.recordAdministrationReceipt(t.Context(), session, mutation.Metadata{OrganizationID: organization, RetryIdentity: "retry", InputHash: "input"}, "succeeded", operatoradmin.Output{ResourceID: organization, Data: json.RawMessage(`{"precise":9007199254740993}`)}); err != nil {
					t.Fatal(err)
				}
				for _, revoked := range []bool{false, true} {
					id := organization
					var revokedAt any
					if revoked {
						id += "_revoked"
						revokedAt = formatTime(now)
					}
					if _, err := s.auth.store.db.ExecContext(t.Context(), `INSERT INTO access_keys(id,token_hash,name,kind,owner_subject,owner_email,permission,access_context,organizations_json,created_at,revoked_at) VALUES(?,?,?,'personal','user_alice','alice@example.test','write','selected',?,?,?)`, id, "key_"+id, "Example", `[{"organization_id":"`+organization+`","project_access":"all"}]`, formatTime(now), revokedAt); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := s.auth.revokeSession(t.Context(), "hash_org_beta"); err != nil {
				t.Fatal(err)
			}
			r, source := platformSQLiteImport(t, s, mode)
			i, err := r.NewImporter(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			for {
				progress, err := i.Step(t.Context(), 1)
				if err != nil {
					t.Fatal(err)
				}
				if progress.Done {
					break
				}
			}
			for _, organization := range []string{"org_alpha", "org_beta"} {
				before, beforeErr := s.auth.session(t.Context(), "hash_"+organization)
				after, afterErr := r.Session(t.Context(), "hash_"+organization)
				if (beforeErr == nil) != (afterErr == nil) {
					t.Fatalf("session parity: SQLite=%v PostgreSQL=%v", beforeErr, afterErr)
				}
				if beforeErr == nil {
					before.Identity.CreatedAt = before.Identity.CreatedAt.UTC()
					before.Identity.ExpiresAt = before.Identity.ExpiresAt.UTC()
					after.Identity.CreatedAt = after.Identity.CreatedAt.UTC()
					after.Identity.ExpiresAt = after.Identity.ExpiresAt.UTC()
					if before.Identity != after.Identity {
						t.Fatalf("identity changed: %+v %+v", before.Identity, after.Identity)
					}
					authorization, err := s.auth.authorization(t.Context(), before, organization)
					if err != nil {
						t.Fatal(err)
					}
					want, err := s.auth.tokens(authorization)
					if err != nil {
						t.Fatal(err)
					}
					got, err := r.Tokens(t.Context(), before.Hash, tracker.OrganizationID(organization), authorization.Binding)
					if err != nil || got != want {
						t.Fatalf("token parity: %+v %+v %v", want, got, err)
					}
				} else if !errors.Is(afterErr, platformstore.ErrNoSession) {
					t.Fatalf("revoked imported session: %v", afterErr)
				}
				for _, revoked := range []bool{false, true} {
					id := organization
					if revoked {
						id += "_revoked"
					}
					before, err := s.auth.accessKey(t.Context(), "token_hash", "key_"+id)
					if err != nil {
						t.Fatal(err)
					}
					_, err = r.KeyAuthority(t.Context(), "key_"+id, tracker.OrganizationID(organization), "project")
					if before.active(now) != (err == nil) {
						t.Fatalf("key authority parity: active=%v PostgreSQL=%v", before.active(now), err)
					}
					if revoked && !errors.Is(err, platformstore.ErrNoSession) {
						t.Fatalf("revoked imported token authorizes claim: %v", err)
					}
				}
				beforeLogin, err := s.auth.consumeTransaction(t.Context(), "login_"+organization, "transaction_"+organization)
				if err != nil {
					t.Fatal(err)
				}
				afterLogin, err := r.ConsumeLogin(t.Context(), "login_"+organization, "transaction_"+organization)
				if err != nil || beforeLogin.Verifier != afterLogin.Verifier || beforeLogin.InvitationToken != afterLogin.InvitationToken {
					t.Fatalf("login parity: %v", err)
				}
			}
		})
	}
}

func platformSQLiteImport(t *testing.T, s *Service, mode string) (*platformstore.Repository, platformstore.ImportSource) {
	t.Helper()
	admin := entryPostgresTemplate.Database(t)
	name := "example_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	password := uuid.NewString()
	quoted := pq.QuoteIdentifier(name)
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pq.QuoteLiteral(password)+" IN ROLE "+postgres.PlatformRoleName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, ok := t.Deadline()
		if !ok {
			t.Error("missing process deadline")
			return
		}
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	pool := entryPostgresTemplate.OpenRole(t, admin, name, password)
	keys := s.auth.keys
	if mode == "retained keyring" {
		var err error
		keys, err = hubsecrets.FromEnvironment(entrySecretEnvironment(2, 1, 2))
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := platformstore.New(t.Context(), pool, platformstore.Config{Pool: platformstore.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}, SigningKey: s.config.SigningKey, Keys: keys, Now: s.config.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	catalog, err := storagecatalog.Read(os.DirFS("../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	registryCopy := filepath.Join(t.TempDir(), "registry.db")
	authCopy := filepath.Join(t.TempDir(), "auth.db")
	for _, copy := range []struct {
		db   *sql.DB
		path string
	}{{s.registry.store.db.DB, registryCopy}, {s.auth.store.db.DB, authCopy}} {
		if _, err := copy.db.ExecContext(t.Context(), "VACUUM INTO ?", copy.path); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(copy.path, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	copies, err := platformstore.OpenSQLiteCopySource(t.Context(), registryCopy, authCopy, catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := copies.Close(); err != nil {
			t.Error(err)
		}
	})
	source := copies.ImportSource()
	source.SigningKey = s.config.SigningKey.Public().(ed25519.PublicKey)
	source.KeyVersions = s.auth.keys.Versions()
	source.Objects = s.attachments
	return r, source
}
