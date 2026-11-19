package hubfixture

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (h *ConversationHostedEntry) AttachmentDeletionRequest(t *testing.T, audience tracker.OrganizationID, id string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/internal/v1/attachments/deleted", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	assertionID, err := cloudassert.NewID()
	if err != nil {
		t.Fatal(err)
	}
	entry, now := h.Config.Hosted.SharedEntry, h.Now()
	assertion, err := cloudassert.Sign(h.signer, cloudassert.Claims{
		Issuer: entry.Issuer, Audience: string(audience), Generation: entry.Generation, Kind: cloudassert.KindService,
		Method: request.Method, Path: request.URL.RequestURI(), BodyDigest: cloudassert.BodyDigest(body),
		IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: assertionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(cloudassert.Header, assertion)
	response := httptest.NewRecorder()
	h.Handler().ServeHTTP(response, request)
	return response
}
