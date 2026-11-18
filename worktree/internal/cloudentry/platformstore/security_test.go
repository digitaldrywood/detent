package platformstore

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
)

func testKeys(t *testing.T) *hubsecrets.Keyring {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		switch name {
		case "DETENT_HUB_SECRET_KEYS":
			return `{"1":"` + key + `"}`
		case "DETENT_HUB_SECRET_KEY_VERSION":
			return "1"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestStoredCredentialsRemainConfidential(t *testing.T) {
	for _, mode := range []string{"legacy signing key", "versioned keyring"} {
		t.Run(mode, func(t *testing.T) {
			db := template.Database(t)
			cfg := testConfig(t)
			if mode == "versioned keyring" {
				cfg.Keys = testKeys(t)
			}
			r := repository(t, db, cfg)
			seedOrganizations(t, r)
			s := createSession(t, r, "first")
			a := authorize(t, r, s, "org_first", "initial")
			other := authorize(t, r, s, "org_second", "other")
			var access, refresh string
			if err := db.QueryRowContext(t.Context(), `SELECT access_token,refresh_token FROM platform.authorizations WHERE organization_id=$1 AND binding=$2`, a.OrganizationID, a.Binding).Scan(&access, &refresh); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(access, "access_initial") || strings.Contains(refresh, "refresh_initial") {
				t.Fatal("plaintext provider credential stored")
			}
			if cfg.Keys != nil && (!strings.HasPrefix(access, "hk1.1.") || !strings.HasPrefix(refresh, "hk1.1.")) {
				t.Fatal("key version envelope lost")
			}
			reopened := replicaRepository(t, r, cfg)
			got, err := reopened.Tokens(t.Context(), s.Hash, a.OrganizationID, a.Binding)
			if err != nil || got != (auth.HostedTokens{AccessToken: "access_initial", RefreshToken: "refresh_initial"}) {
				t.Fatalf("restart: %+v %v", got, err)
			}
			if cfg.Keys != nil {
				missingKeyConfig := cfg
				missingKeyConfig.Keys = nil
				missingKey := replicaRepository(t, r, missingKeyConfig)
				if _, err := missingKey.Tokens(t.Context(), s.Hash, a.OrganizationID, a.Binding); !errors.Is(err, hubsecrets.ErrUnavailable) || errors.Is(err, ErrNoSession) {
					t.Fatalf("unavailable encryption key = %v, want operational error", err)
				}
			}
			if _, err := db.ExecContext(t.Context(), `UPDATE platform.authorizations SET access_token=$1 WHERE organization_id=$2 AND binding=$3`, access, other.OrganizationID, other.Binding); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Tokens(t.Context(), s.Hash, other.OrganizationID, other.Binding); !errors.Is(err, ErrNoSession) {
				t.Fatalf("ciphertext transplanted across tenants: %v", err)
			}
			if cfg.Keys != nil {
				if _, err := db.ExecContext(t.Context(), `UPDATE platform.authorizations SET access_token=$1 WHERE organization_id=$2 AND binding=$3`, refresh, a.OrganizationID, a.Binding); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Tokens(t.Context(), s.Hash, a.OrganizationID, a.Binding); !errors.Is(err, ErrNoSession) {
					t.Fatalf("ciphertext transplanted across purposes: %v", err)
				}
			}
			for _, owner := range []string{"first", "second"} {
				if err := r.StoreGitHubToken(t.Context(), owner, "connection_"+owner, "github_secret_"+owner, nil); err != nil {
					t.Fatal(err)
				}
				var ciphertext string
				if err := db.QueryRowContext(t.Context(), `SELECT access_token FROM platform.github_user_connections WHERE owner_subject=$1`, owner).Scan(&ciphertext); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(ciphertext, "github_secret_") {
					t.Fatal("GitHub token stored in plaintext")
				}
				token, err := reopened.GitHubToken(t.Context(), owner)
				if err != nil || token != "github_secret_"+owner {
					t.Fatalf("GitHub token: %q %v", token, err)
				}
				if err := r.RevokeGitHubToken(t.Context(), owner, "connection_"+owner); err != nil {
					t.Fatal(err)
				}
				if _, err := r.GitHubToken(t.Context(), owner); !errors.Is(err, ErrNoSession) {
					t.Fatalf("revoked GitHub token: %v", err)
				}
			}
		})
	}
}

func TestInvalidAuthorizationNeverReturnsPartialCredentials(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	s := createSession(t, r, "first")
	a := authorize(t, r, s, "org_first", "initial")
	if _, err := db.ExecContext(t.Context(), `UPDATE platform.authorizations SET refresh_token='invalid-ciphertext' WHERE organization_id=$1 AND binding=$2`, a.OrganizationID, a.Binding); err != nil {
		t.Fatal(err)
	}
	tokens, err := r.Tokens(t.Context(), s.Hash, a.OrganizationID, a.Binding)
	if !errors.Is(err, ErrNoSession) || tokens != (auth.HostedTokens{}) {
		t.Fatalf("partial credentials returned: %+v %v", tokens, err)
	}
}
