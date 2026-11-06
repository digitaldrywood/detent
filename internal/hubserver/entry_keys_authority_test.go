package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func TestEntryKeyCallReusesRequestAuthentication(t *testing.T) {
	for _, authenticated := range []bool{true, false} {
		t.Run(map[bool]string{true: "authenticated", false: "missing authority"}[authenticated], func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"name": operatortool.WorkList, "arguments": map[string]any{}, "connection_id": "example"})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/keys/call", bytes.NewReader(body))
			request.Header.Set(echo.HeaderAuthorization, "Bearer example")
			recorder := httptest.NewRecorder()
			c := echo.New().NewContext(request, recorder)
			c.SetPath("/internal/v1/keys/call")
			c.SetParamNames("organization")
			c.SetParamValues("first")
			probe := &authorityReaderProbe{err: tenantauth.ErrUnavailable}
			service := &Service{config: Config{TenantRepository: probe, now: time.Now, Logger: slog.New(slog.DiscardHandler)}}
			if authenticated {
				c.Set("hub_api_credential", apiCredential{ID: "key", Scope: apiScopeOperator, EntryKey: &apikey.KeyAuthority{ID: "key"}, HostedKeyScope: "invalid"})
			}
			err = service.entryKeyCall(c)
			if !authenticated {
				if err == nil && recorder.Code != http.StatusForbidden {
					t.Fatalf("missing authority: status=%d body=%s", recorder.Code, recorder.Body)
				}
				if err != nil && !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatal(err)
				}
			} else if err != nil || recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "key_permission_denied") {
				t.Fatalf("request authority not reused: status=%d err=%v body=%s", recorder.Code, err, recorder.Body)
			}
			if probe.reads != 0 || probe.legacyReads != 0 {
				t.Fatalf("request was reauthenticated: native=%d legacy=%d", probe.reads, probe.legacyReads)
			}
		})
	}
}
