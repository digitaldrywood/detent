package cli

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestCheckDoctorFleetStalenessSurfacesLiveFaults(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"status":"ok",
			"mode":"running",
			"checks":{"hub":"configured","store":"configured","registry":"configured","connector":"configured"},
			"staleness_warnings":[
				{"id":"warning-1","class":"fault","project_id":"detent","kind":"stranded_park","identifier":"digitaldrywood/detent#1574","reason":"park cannot recover","detail":"stale"},
				{"id":"diagnostic-1","class":"diagnostic","project_id":"detent","kind":"lane_aging","identifier":"digitaldrywood/detent#1575","reason":"lane threshold exceeded","detail":"aged"},
				{"id":"review-1","class":"review_queue","project_id":"detent","kind":"human_gate","identifier":"digitaldrywood/detent#1576","reason":"waiting for review","detail":"review"}
			]
		}`))
	}))
	t.Cleanup(server.Close)
	host, portText := splitStalenessTestServerAddress(t, server)
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	check := checkDoctorFleetStaleness(t.Context(), BootConfig{Host: host, Port: &port}, "detent", doctorDeps{httpDo: server.Client().Do}.withDefaults())
	if check.Status != doctorWarn || len(check.StalenessWarnings) != 1 {
		t.Fatalf("check = %#v, want one fault", check)
	}
	if !strings.Contains(check.Detail, "digitaldrywood/detent#1574") {
		t.Fatalf("detail = %q, want issue identifier", check.Detail)
	}

	filtered := checkDoctorFleetStaleness(t.Context(), BootConfig{Host: host, Port: &port}, "other", doctorDeps{httpDo: server.Client().Do}.withDefaults())
	if filtered.Status != doctorOK || len(filtered.StalenessWarnings) != 0 {
		t.Fatalf("filtered check = %#v, want OK", filtered)
	}
}

func TestDoctorStalenessWarningDetailFallsBackToProject(t *testing.T) {
	t.Parallel()
	if got := doctorStalenessWarningDetail(telemetry.StalenessWarning{ProjectID: "detent", Reason: "not advancing"}); got != "detent not advancing" {
		t.Fatalf("doctorStalenessWarningDetail() = %q", got)
	}
}

func splitStalenessTestServerAddress(t *testing.T, server *httptest.Server) (string, string) {
	t.Helper()
	address := strings.TrimPrefix(server.URL, "http://")
	index := strings.LastIndex(address, ":")
	if index < 0 {
		t.Fatalf("server address = %q", address)
	}
	return address[:index], address[index+1:]
}
