package cli

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestCheckDoctorHealthNotificationDelivery(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"status":"needs_attention",
			"mode":"running",
			"checks":{"hub":"configured","store":"configured","registry":"configured","connector":"configured"},
			"health_notification_failures":[
				{"event_id":"health-1","identity":"project:detent:dispatch_stall","scope":"project","project_id":"detent","transition":"entry","attempts":5,"max_attempts":5,"last_error":"receiver unavailable","failed_at":"2026-08-13T18:00:00Z"},
				{"event_id":"health-2","identity":"fleet","scope":"fleet","transition":"entry","attempts":1,"max_attempts":5,"last_error":"receiver unavailable","next_attempt_at":"2026-08-13T18:01:00Z"}
			]
		}`))
	}))
	t.Cleanup(server.Close)
	host, portText := splitStalenessTestServerAddress(t, server)
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	check := checkDoctorHealthNotificationDelivery(t.Context(), BootConfig{Host: host, Port: &port}, "detent", doctorDeps{httpDo: server.Client().Do}.withDefaults())
	if check.Status != doctorWarn || len(check.HealthNotificationFailures) != 2 {
		t.Fatalf("check = %#v, want project and fleet failures", check)
	}
	if !strings.Contains(check.Detail, "1 exhausted") {
		t.Fatalf("detail = %q, want exhausted count", check.Detail)
	}

	filtered := checkDoctorHealthNotificationDelivery(t.Context(), BootConfig{Host: host, Port: &port}, "other", doctorDeps{httpDo: server.Client().Do}.withDefaults())
	if filtered.Status != doctorWarn || len(filtered.HealthNotificationFailures) != 1 || filtered.HealthNotificationFailures[0].Scope != "fleet" {
		t.Fatalf("filtered = %#v, want fleet failure", filtered)
	}
}
