package cli

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestCheckDoctorLaneSignalsListsAffectedOpenIssues(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"status":"needs_attention",
			"mode":"running",
			"checks":{"hub":"configured","store":"configured","registry":"configured","connector":"configured"},
			"lane_signal_warnings":[
				{"reason_code":"lane_signal_ignored","project_id":"detent","issue_id":"issue-label","identifier":"owner/repo#1","configured_source":"ProjectV2 Status","signal_kind":"label","signal":"detent:todo","action":"set Status to Todo"},
				{"reason_code":"lane_signal_ignored","project_id":"detent","issue_id":"issue-status","identifier":"owner/repo#2","configured_source":"labels with prefix detent:","signal_kind":"status","signal":"Status Todo","action":"apply label detent:todo"},
				{"reason_code":"lane_signal_ignored","project_id":"other","issue_id":"issue-other","identifier":"owner/other#3","configured_source":"labels with prefix detent:","action":"apply label detent:todo"}
			]
		}`))
	}))
	t.Cleanup(server.Close)
	host, portText := splitStalenessTestServerAddress(t, server)
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	check := checkDoctorLaneSignals(t.Context(), BootConfig{Host: host, Port: &port}, "detent", doctorDeps{httpDo: server.Client().Do}.withDefaults())
	if check.Status != doctorWarn || len(check.LaneSignalWarnings) != 2 {
		t.Fatalf("check = %#v, want two warnings", check)
	}
	for _, want := range []string{"owner/repo#1 reads lanes from ProjectV2 Status; set Status to Todo", "owner/repo#2 reads lanes from labels with prefix detent:; apply label detent:todo"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("detail %q missing %q", check.Detail, want)
		}
	}

	filtered := checkDoctorLaneSignals(t.Context(), BootConfig{Host: host, Port: &port}, "missing", doctorDeps{httpDo: server.Client().Do}.withDefaults())
	if filtered.Status != doctorOK || len(filtered.LaneSignalWarnings) != 0 {
		t.Fatalf("filtered check = %#v, want OK", filtered)
	}
}
