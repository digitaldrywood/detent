package platformstore

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func createOAuthGrant(t *testing.T, r *Repository, org tracker.OrganizationID, id string) (OAuthGrant, OAuthRequest) {
	t.Helper()
	client := "client_" + string(org) + "_" + id
	redirect := "https://example.com/callback"
	if err := r.CreateOAuthClient(t.Context(), OAuthClient{ID: client, Name: "Example", AuthMethod: "none", RedirectURIs: []string{redirect, "https://example.com/other"}}); err != nil {
		t.Fatal(err)
	}
	grant := OAuthGrant{OrganizationID: org, ID: id, ClientID: client, OwnerSubject: "owner_" + string(org), OwnerEmail: "owner@example.com", Permission: "write", Resource: "https://example.com/mcp"}
	request := OAuthRequest{ID: "request_" + client, RedirectURI: redirect, Challenge: oauth2.S256ChallengeFromVerifier("example-verifier"), CodeHash: "code_" + client, CodeExpiresAt: r.now().Add(time.Minute)}
	if err := r.CreateOAuthGrant(t.Context(), grant, request); err != nil {
		t.Fatal(err)
	}
	return grant, request
}

func oauthTokens(r *Repository, id string) OAuthTokens {
	return OAuthTokens{AccessHash: "access_" + id, RefreshHash: "refresh_" + id, AccessExpiresAt: r.now().Add(time.Hour), RefreshExpiresAt: r.now().Add(24 * time.Hour)}
}

func exchange(t *testing.T, r *Repository, g OAuthGrant, request OAuthRequest, tokens OAuthTokens) {
	t.Helper()
	if err := r.ExchangeOAuthCode(t.Context(), g.OrganizationID, request.CodeHash, g.ClientID, g.Resource, request.RedirectURI, "example-verifier", tokens); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthExchangeScopeAndRevocation(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, id := range []string{"first", "second"} {
			t.Run(string(org)+"/"+id, func(t *testing.T) {
				g, request := createOAuthGrant(t, r, org, id)
				tokens := oauthTokens(r, string(org)+id)
				for _, tc := range []struct {
					name   string
					change func(*OAuthGrant, *OAuthRequest, *string)
				}{
					{"wrong tenant", func(g *OAuthGrant, _ *OAuthRequest, _ *string) { g.OrganizationID = "org_missing" }},
					{"wrong client", func(g *OAuthGrant, _ *OAuthRequest, _ *string) { g.ClientID = "wrong" }},
					{"wrong resource", func(g *OAuthGrant, _ *OAuthRequest, _ *string) { g.Resource = "https://other.example/mcp" }},
					{"wrong redirect", func(_ *OAuthGrant, q *OAuthRequest, _ *string) { q.RedirectURI = "https://other.example/callback" }},
					{"wrong verifier", func(_ *OAuthGrant, _ *OAuthRequest, v *string) { *v = "wrong" }},
				} {
					t.Run(tc.name, func(t *testing.T) {
						grant, q := g, request
						verifier := "example-verifier"
						tc.change(&grant, &q, &verifier)
						err := r.ExchangeOAuthCode(t.Context(), grant.OrganizationID, q.CodeHash, grant.ClientID, grant.Resource, q.RedirectURI, verifier, tokens)
						if !errors.Is(err, ErrNoSession) {
							t.Fatalf("exchange: %v", err)
						}
					})
				}
				exchange(t, r, g, request, tokens)
				if err := r.ExchangeOAuthCode(t.Context(), g.OrganizationID, request.CodeHash, g.ClientID, g.Resource, request.RedirectURI, "example-verifier", tokens); !errors.Is(err, ErrNoSession) {
					t.Fatalf("code replay: %v", err)
				}
				stored, err := r.OAuthGrant(t.Context(), org, tokens.AccessHash)
				if err != nil || stored.ID != id || stored.Permission != "write" || stored.OwnerSubject != g.OwnerSubject {
					t.Fatalf("grant: %+v %v", stored, err)
				}
				other := tracker.OrganizationID("org_first")
				if org == other {
					other = "org_second"
				}
				if _, err := r.OAuthGrant(t.Context(), other, tokens.AccessHash); !errors.Is(err, ErrNoSession) {
					t.Fatalf("cross tenant token: %v", err)
				}
				if err := r.RevokeOAuthGrant(t.Context(), other, g.OwnerSubject, id); err != nil {
					t.Fatal(err)
				}
				if _, err := r.OAuthGrant(t.Context(), org, tokens.AccessHash); err != nil {
					t.Fatalf("cross tenant revocation affected grant: %v", err)
				}
				rotated := oauthTokens(r, string(org)+id+"_rotated")
				if err := r.RotateOAuthRefresh(t.Context(), org, tokens.RefreshHash, g.ClientID, g.Resource, rotated); err != nil {
					t.Fatal(err)
				}
				if _, err := r.OAuthGrant(t.Context(), org, tokens.AccessHash); !errors.Is(err, ErrNoSession) {
					t.Fatalf("old access token active: %v", err)
				}
				if _, err := r.OAuthGrant(t.Context(), org, rotated.AccessHash); err != nil {
					t.Fatal(err)
				}
				if err := r.RotateOAuthRefresh(t.Context(), org, tokens.RefreshHash, g.ClientID, g.Resource, oauthTokens(r, "replay_"+string(org)+id)); !errors.Is(err, ErrNoSession) {
					t.Fatalf("refresh replay: %v", err)
				}
				if _, err := r.OAuthGrant(t.Context(), org, rotated.AccessHash); !errors.Is(err, ErrNoSession) {
					t.Fatalf("replayed grant active: %v", err)
				}
			})
		}
	}
}

func TestConcurrentOAuthMutation(t *testing.T) {
	for _, operation := range []string{"code exchange", "refresh replay", "revocation"} {
		t.Run(operation, func(t *testing.T) {
			db := template.Database(t)
			cfg := testConfig(t)
			first := repository(t, db, cfg)
			second := replicaRepository(t, first, cfg)
			seedOrganizations(t, first)
			g, request := createOAuthGrant(t, first, "org_first", "shared")
			other, q := createOAuthGrant(t, first, "org_second", "shared")
			otherTokens := oauthTokens(first, "other")
			exchange(t, first, other, q, otherTokens)
			initial := oauthTokens(first, "initial")
			if operation != "code exchange" {
				exchange(t, first, g, request, initial)
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			var workers sync.WaitGroup
			for i, r := range []*Repository{first, second} {
				workers.Go(func() {
					<-start
					next := oauthTokens(r, fmt.Sprintf("next_%d", i))
					switch operation {
					case "code exchange":
						results <- r.ExchangeOAuthCode(t.Context(), g.OrganizationID, request.CodeHash, g.ClientID, g.Resource, request.RedirectURI, "example-verifier", next)
					case "refresh replay":
						results <- r.RotateOAuthRefresh(t.Context(), g.OrganizationID, initial.RefreshHash, g.ClientID, g.Resource, next)
					case "revocation":
						if i == 0 {
							results <- r.RevokeOAuthGrant(t.Context(), g.OrganizationID, g.OwnerSubject, g.ID)
						} else {
							results <- r.RotateOAuthRefresh(t.Context(), g.OrganizationID, initial.RefreshHash, g.ClientID, g.Resource, next)
						}
					}
				})
			}
			close(start)
			workers.Wait()
			close(results)
			success, refused := 0, 0
			for err := range results {
				if err == nil {
					success++
				} else if errors.Is(err, ErrNoSession) {
					refused++
				} else {
					t.Fatal(err)
				}
			}
			if operation == "code exchange" || operation == "refresh replay" {
				if success != 1 || refused != 1 {
					t.Fatalf("success=%d refused=%d", success, refused)
				}
			}
			if operation != "code exchange" {
				for _, hash := range []string{initial.AccessHash, "access_next_0", "access_next_1"} {
					if _, err := first.OAuthGrant(t.Context(), g.OrganizationID, hash); !errors.Is(err, ErrNoSession) {
						t.Fatalf("grant survived %s: %v", operation, err)
					}
				}
			}
			if _, err := first.OAuthGrant(t.Context(), other.OrganizationID, otherTokens.AccessHash); err != nil {
				t.Fatalf("other tenant affected: %v", err)
			}
		})
	}
}

func TestOAuthRefreshCannotReuseAHash(t *testing.T) {
	db := template.Database(t)
	r := repository(t, db, testConfig(t))
	seedOrganizations(t, r)
	g, request := createOAuthGrant(t, r, "org_first", "first")
	initial := oauthTokens(r, "initial")
	exchange(t, r, g, request, initial)
	if err := r.RotateOAuthRefresh(t.Context(), g.OrganizationID, initial.RefreshHash, g.ClientID, g.Resource, initial); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reused current hash: %v", err)
	}
	rotated := oauthTokens(r, "rotated")
	if err := r.RotateOAuthRefresh(t.Context(), g.OrganizationID, initial.RefreshHash, g.ClientID, g.Resource, rotated); err != nil {
		t.Fatal(err)
	}
	if err := r.RotateOAuthRefresh(t.Context(), g.OrganizationID, rotated.RefreshHash, g.ClientID, g.Resource, initial); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reused spent hash: %v", err)
	}
	if _, err := r.OAuthGrant(t.Context(), g.OrganizationID, rotated.AccessHash); err != nil {
		t.Fatalf("invalid replacement changed grant: %v", err)
	}
}
