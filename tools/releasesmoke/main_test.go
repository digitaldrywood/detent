package main

import "testing"

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
		{"RPC failure", result{Status: 200, Expected: 200, Body: `{"error":{"code":-32603}}`}, "FAIL"},
		{"pending projection", result{Status: 200, Expected: 200, Board: true, Body: `{"projection_pending":true,"rows":[]}`}, "FAIL"},
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
