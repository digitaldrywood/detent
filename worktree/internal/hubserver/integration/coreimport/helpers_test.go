package coreimport_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const testTimestamp = "2026-09-01T12:00:00Z"

func TestMain(m *testing.M) { hubtest.Main(m) }

type duplicateChallenge struct {
	PossibleDuplicates []hubscope.PossibleDuplicate `json:"possible_duplicates"`
	ChallengeToken     string                       `json:"challenge_token"`
}

func seedHubAPIToken(t *testing.T, h *hubfixture.Hub, id string, token string, scope hubapi.APIScope) {
	t.Helper()
	hash := apikey.HashToken(token)
	now := h.Config.Clock().UTC().Truncate(time.Microsecond)
	if err := h.Config.TenantRepository.WriteTenant(t.Context(), h.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: h.Primary, ID: id, Name: id, Hash: hash, Fingerprint: hubapi.TokenFingerprint(hash), Scope: string(scope), Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatalf("insert Hub API token: %v", err)
	}
}

func parseTestTime(t *testing.T, raw string) time.Time {
	t.Helper()
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func createIndependentNativeIssue(t *testing.T, request tracker.CreateIssue, send func(tracker.CreateIssue) *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	if !strings.Contains(request.Body, "## Acceptance criteria") {
		request.Body += hubfixture.IssueContractSections
	}
	response := send(request)
	if response.Code == http.StatusConflict {
		var failure hubapi.APIErrorResponse
		hubfixture.Decode(t, response, &failure)
		var challenge duplicateChallenge
		hubfixture.Decode(t, response, &challenge)
		if failure.Code != "possible_duplicates" || challenge.ChallengeToken == "" || len(challenge.PossibleDuplicates) == 0 {
			t.Fatalf("independent fixture challenge: %s", response.Body.String())
		}
		request.ChallengeToken = challenge.ChallengeToken
		for _, match := range challenge.PossibleDuplicates {
			request.NotDuplicateOf = append(request.NotDuplicateOf, match.Number)
		}
		response = send(request)
	}
	hubfixture.RequireStatus(t, response, http.StatusOK)
	return response
}

func historyTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
