package scenariotest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIncorporationProbesPreserveRequestBudget(t *testing.T) {
	for blockedProbe := 1; blockedProbe <= 6; blockedProbe++ {
		t.Run(fmt.Sprintf("probe_%d", blockedProbe), func(t *testing.T) {
			payload, err := json.Marshal(tracker.LandChangeVersion{
				IncorporationObjects: []byte{1},
				Receipt:              &tracker.NativeLandingReceipt{SecurityAudit: &tracker.SecurityAuditReview{}, Incorporation: &tracker.LandingIncorporation{}},
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://hub.example.test/landing", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			probes, landed := 0, 0
			refused := false
			checked := map[string]bool{}
			budgetBody := `{"code":"runner_request_budget"}`
			transport := incorporationRejectionTransport{t: t, next: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				var candidate tracker.LandChangeVersion
				if err := json.NewDecoder(request.Body).Decode(&candidate); err != nil {
					return nil, err
				}
				status, body := http.StatusUnprocessableEntity, "{}"
				if strings.HasPrefix(candidate.IdempotencyKey, "reject-") {
					probes++
					if probes == blockedProbe && !refused {
						refused = true
						status, body = http.StatusTooManyRequests, budgetBody
					} else {
						checked[candidate.IdempotencyKey] = true
					}
				} else {
					landed++
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"1"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusTooManyRequests || string(body) != budgetBody || response.Header.Get("Retry-After") != "1" || probes != blockedProbe || landed != 0 {
				t.Fatalf("budget refusal lost: response=%+v body=%s probes=%d landed=%d error=%v", response, body, probes, landed, readErr)
			}
			response, err = transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK || len(checked) != 6 || landed != 1 {
				t.Fatalf("retry omitted rejection checks: status=%d checked=%v landed=%d", response.StatusCode, checked, landed)
			}
		})
	}
}
