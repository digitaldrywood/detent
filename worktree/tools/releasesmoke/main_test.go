package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestFailedJourneysCannotPass(t *testing.T) {
	for _, test := range []struct {
		name string
		row  result
		want string
	}{
		{"success", result{Status: 200, Expected: 200, Body: `{"items":[]}`}, "PASS"},
		{"missing attempt", result{Status: 404, Expected: 404, Body: `{"error":"not found"}`}, "PASS"},
		{"attempt regression", result{Status: 500, Expected: 404}, "FAIL"},
		{"redirect to login", result{Status: 302, Expected: 200}, "FAIL"},
		{"tool failure with HTTP success", result{Status: 200, Expected: 200, Body: `{"result":{"isError":true}}`}, "FAIL"},
		{"application failure", result{Status: 200, Expected: 200, Body: `{"error":"request failed"}`}, "FAIL"},
		{"structured application failure", result{Status: 200, Expected: 200, Body: `{"result":{"structuredContent":{"error":"request failed"}}}`}, "FAIL"},
		{"historical attempt failures", result{Status: 200, Expected: 200, Body: `{"items":[{"data":{"terminal_failure":{"error":"provider failed"},"finalization":{"error":"push failed"}}}]}`}, "PASS"},
		{"MCP historical attempt failures", result{Status: 200, Expected: 200, Body: `{"result":{"isError":false,"structuredContent":{"data":{"terminal_failure":{"error":"provider failed"},"finalization":{"error":"push failed"}}}}}`}, "PASS"},
		{"RPC failure", result{Status: 200, Expected: 200, Body: `{"error":{"code":-32603}}`}, "FAIL"},
		{"pending projection", result{Status: 200, Expected: 200, Board: true, Body: `{"projection_pending":true,"rows":[]}`}, "FAIL"},
		{"nested pending projection", result{Status: 200, Expected: 200, Board: true, Body: `{"data":{"rows":[{"status":"projection_pending"}]}}`}, "FAIL"},
		{"board historical failure", result{Status: 200, Expected: 200, Board: true, Body: `{"rows":[{"error":"previous attempt failed"}]}`}, "PASS"},
		{"ready projection", result{Status: 200, Expected: 200, Board: true, Body: `{"projection_pending":false,"rows":[]}`}, "PASS"},
		{"ordinary pending verification", result{Status: 200, Expected: 200, Body: `{"verification":"pending"}`}, "PASS"},
		{"missing runner", result{Skipped: true}, "SKIPPED"},
		{"loop", result{Status: 200, Expected: 200, Error: "request count grew"}, "FAIL"},
		{"invalid JSON", result{Status: 200, Expected: 200, Body: `<html>login</html>`}, "FAIL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classify(test.row); got != test.want {
				t.Fatalf("classification = %s, want %s", got, test.want)
			}
		})
	}
}

func TestReportOmitsUntrustedText(t *testing.T) {
	secret := "FAKE_BROWSER_SESSION_SECRET_1234567890"
	for _, test := range []struct {
		name string
		row  result
		code int
		want string
	}{
		{"failure body", result{Name: "board counts", Status: 500, Expected: 200, Body: secret}, 1, "unexpected or missing HTTP status"},
		{"failure error", result{Name: "browser journeys", Error: secret}, 1, "unexpected or missing HTTP status"},
		{"boundary crossing", result{Name: "issue detail page", Status: 200, Expected: 200, Error: strings.Repeat("x", 190) + secret}, 1, "journey failed"},
		{"body boundary crossing", result{Name: "diagnostics", Status: 200, Expected: 200, Body: strings.Repeat("x", 230) + secret}, 1, "response validation failed"},
		{"check name", result{Name: secret, Status: 200, Expected: 200}, 0, "unrecognized check"},
		{"success note", result{Name: "work_item", Status: 200, Expected: 200, Note: secret}, 0, "work_item"},
		{"skipped note", result{Name: "runner heartbeat identity dry read", Skipped: true, Note: secret}, 0, "SKIPPED"},
		{"tool count", result{Name: "tools/list", Status: 200, Expected: 200, Note: "42 tools"}, 0, "42 tools"},
		{"board count", result{Name: "browser board load 1", Status: 200, Expected: 200, Note: "3 board requests"}, 0, "3 board requests"},
		{"stable counts", result{Name: "board request count stable", Status: 200, Expected: 200, Note: "3/3/3"}, 0, "3/3/3"},
		{"absent runner", result{Name: "runner heartbeat identity dry read", Skipped: true, Note: "no test runner credential; no heartbeat was sent"}, 0, "no test runner credential; no heartbeat was sent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var input, output bytes.Buffer
			if err := json.NewEncoder(&input).Encode([]result{test.row}); err != nil {
				t.Fatal(err)
			}
			code := report(&input, &output)
			if code != test.code {
				t.Fatalf("exit = %d, want %d", code, test.code)
			}
			if strings.Contains(output.String(), "FAKE_") {
				t.Fatalf("report exposes untrusted text: %s", output.String())
			}
			if !strings.Contains(output.String(), test.want) {
				t.Fatalf("report = %q, want %q", output.String(), test.want)
			}
		})
	}
}
