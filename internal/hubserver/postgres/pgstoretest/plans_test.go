package pgstoretest

import (
	"encoding/json"
	"testing"
)

func TestSearchPlanDocumentWorkCountsRoundedLoopAverages(t *testing.T) {
	for _, test := range []struct {
		name string
		plan string
		want float64
	}{
		{"whole rows on PostgreSQL 17", `{"Relation Name":"search_postings","Actual Rows":0,"Actual Loops":18,"Rows Removed by Filter":54}`, 18 * 55},
		{"fractional rows on PostgreSQL 18", `{"Relation Name":"search_postings","Actual Rows":0.50,"Actual Loops":18,"Rows Removed by Filter":54}`, 18 * 55.005},
		{"single loop totals are exact", `{"Relation Name":"search_postings","Actual Rows":9,"Actual Loops":1,"Rows Removed by Filter":3}`, 12},
		{"nested index nodes", `{"Plans":[{"Index Name":"search_postings_pkey","Actual Rows":1.56,"Actual Loops":9}]}`, 9 * 1.565},
		{"unrelated relations", `{"Relation Name":"organizations","Actual Rows":7,"Actual Loops":4}`, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var node SearchPlanNode
			if err := json.Unmarshal([]byte(test.plan), &node); err != nil {
				t.Fatal(err)
			}
			if got := node.DocumentWork(); got < test.want-1e-9 || got > test.want+1e-9 {
				t.Fatalf("DocumentWork() = %v, want %v", got, test.want)
			}
		})
	}
}
