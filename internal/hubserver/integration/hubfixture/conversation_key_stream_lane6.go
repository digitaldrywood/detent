package hubfixture

import (
	"crypto/ed25519"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func OpenConversationKeyStreamEntry(t *testing.T, clock func() time.Time, resolver entryauthority.Resolver) *ConversationHostedEntry {
	t.Helper()
	public, signer, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHostedSecurity(t, func(cfg *Config) {
		cfg.Clock = clock
		cfg.Conversation = &conversations.ConversationConfig{Enabled: true}
		cfg.Hosted.PublicURL = "https://hub.example.test"
		cfg.Hosted.SharedEntry = &hostedplans.HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{public}, Generation: 1, ResolveKey: resolver}
	})
	hub.Config.Hosted.SharedEntry.Verifier().Now = clock
	return &ConversationHostedEntry{Hosted: hub, signer: signer}
}

func (h *ConversationHostedEntry) OpenConversationKeyStreamPeer(t *testing.T, organization tracker.OrganizationID, resolver entryauthority.Resolver) *ConversationHostedEntry {
	t.Helper()
	parent := *h.Hosted
	parentHub := *parent.Hub
	settings := *parentHub.Config.Hosted
	entry := *settings.SharedEntry
	entry.ResolveKey = resolver
	settings.SharedEntry = &entry
	parentHub.Config.Hosted = &settings
	parent.Hub = &parentHub
	peer := OpenConversationHostedPeer(t, &parent, organization)
	peer.Config.Hosted.SharedEntry.Verifier().Now = peer.Now
	return &ConversationHostedEntry{Hosted: peer, signer: h.signer}
}

func (h *ConversationHostedEntry) SignConversationKeyStream(t *testing.T, request *http.Request, user HostedUser, key apikey.KeyAuthority) {
	t.Helper()
	now, entry := h.Now(), h.Config.Hosted.SharedEntry
	id, err := cloudassert.NewID()
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := cloudassert.Sign(h.signer, cloudassert.Claims{
		Issuer: entry.Issuer, Audience: string(h.Primary), Generation: entry.Generation, Kind: cloudassert.KindKey,
		Method: request.Method, Path: request.URL.RequestURI(), BodyDigest: cloudassert.BodyDigest(nil),
		IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id,
		Subject: user.Identity.Subject, ProviderOrganization: h.Config.Hosted.WorkOSOrganizationID, Role: "owner", Key: &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(cloudassert.Header, assertion)
}
