package hubfixture

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
)

type ConversationHostedEntry struct {
	*Hosted
	signer ed25519.PrivateKey
}

func OpenConversationHostedEntry(t *testing.T) *ConversationHostedEntry {
	t.Helper()
	public, signer, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHostedSecurity(t, func(cfg *Config) {
		cfg.Hosted.PublicURL = "https://hub.example.test"
		cfg.Hosted.SharedEntry = &hostedplans.HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{public}, Generation: 1}
	})
	return &ConversationHostedEntry{Hosted: hub, signer: signer}
}

func (h *ConversationHostedEntry) BrowserRequest(t *testing.T, user HostedUser, role, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewReader(body))
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	id, err := cloudassert.NewID()
	if err != nil {
		t.Fatal(err)
	}
	identity := user.Identity.Hosted
	now := h.Now()
	audience := string(h.Primary)
	binding := "shared-" + identity.Subject
	entry := h.Config.Hosted.SharedEntry
	csrf := cloudassert.CSRFToken(binding, audience)
	claims := cloudassert.Claims{
		Issuer: entry.Issuer, Audience: audience, Generation: entry.Generation, Kind: cloudassert.KindBrowser,
		Method: method, Path: req.URL.RequestURI(), BodyDigest: cloudassert.BodyDigest(body),
		IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id,
		Subject: identity.Subject, Email: user.Identity.Email, ProviderOrganization: identity.OrganizationID, ProviderSession: identity.SessionID,
		SessionCreatedAt: identity.CreatedAt, SessionExpiresAt: identity.ExpiresAt,
		SupportActor: identity.SupportActor, SupportReason: identity.SupportReason, Role: role, AccessExpiresAt: now.Add(5 * time.Minute),
		Binding: cloudassert.AuthorizationBinding(binding, audience, identity.SessionID), CSRF: csrf,
	}
	assertion, err := cloudassert.Sign(h.signer, claims)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(cloudassert.Header, assertion)
	req.Header.Set("X-CSRF-Token", csrf)
	response := httptest.NewRecorder()
	h.Handler().ServeHTTP(response, req)
	return response
}
