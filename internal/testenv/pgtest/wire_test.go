package pgtest

import (
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

func TestFrontendTraceChargesRepeatedPreparation(t *testing.T) {
	var trace FrontendTrace
	prepare := &pgproto3.Parse{Name: "stmtcache_example", Query: "BEGIN READ ONLY"}
	describe := &pgproto3.Describe{ObjectType: 'S', Name: prepare.Name}
	bind := &pgproto3.Bind{PreparedStatement: prepare.Name}
	sync := &pgproto3.Sync{}
	for _, step := range []struct {
		name     string
		messages []pgproto3.FrontendMessage
		want     WireCost
	}{
		{"first preparation", []pgproto3.FrontendMessage{prepare, describe, sync}, WireCost{FirstPrepareExchanges: 1}},
		{"cached execution", []pgproto3.FrontendMessage{bind, &pgproto3.Execute{}, sync}, WireCost{Statements: 1, Transactions: 1, ExecutionExchanges: 1}},
		{"repeated preparation", []pgproto3.FrontendMessage{prepare, describe, sync}, WireCost{ExecutionExchanges: 1}},
		{"extra sync", []pgproto3.FrontendMessage{sync}, WireCost{ExecutionExchanges: 1}},
		{"unnamed execution", []pgproto3.FrontendMessage{&pgproto3.Parse{Query: "SELECT 1"}, &pgproto3.Bind{}, &pgproto3.Execute{}, sync}, WireCost{Statements: 1, ExecutionExchanges: 1}},
	} {
		var got WireCost
		for _, msg := range step.messages {
			frame, err := msg.Encode(nil)
			if err != nil {
				t.Fatal(err)
			}
			cost := trace.Count(frame)
			got.FirstPrepareExchanges += cost.FirstPrepareExchanges
			got.ExecutionExchanges += cost.ExecutionExchanges
			got.Statements += cost.Statements
			got.Transactions += cost.Transactions
		}
		if got != step.want {
			t.Fatalf("%s: got %+v, want %+v", step.name, got, step.want)
		}
	}
}
