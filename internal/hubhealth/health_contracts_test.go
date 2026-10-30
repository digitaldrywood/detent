package hubhealth

import (
	"context"
	"strconv"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthContractsBoundedRetention(t *testing.T) {
	t.Parallel()
	var issue tracker.NativeIssue
	issue.WorkItemID = "wi_example"
	issue.Revision = 1
	scope := Scope{Organization: "org_example", Project: "project_example"}
	key := contractKey{Organization: scope.Organization, Project: scope.Project, Item: string(issue.WorkItemID)}
	cache := ContractCache{entries: make(map[contractKey]contractResult)}
	for i := range ReadLimit {
		old := contractKey{Item: strconv.Itoa(i)}
		cache.order = append(cache.order, old)
		cache.entries[old] = contractResult{}
	}
	load := func(context.Context, hubsql.Queryer, Scope, string) (ContractObservation, error) {
		return ContractObservation{Issue: issue, Cacheable: true}, nil
	}
	for _, test := range []struct {
		name     string
		revision tracker.Revision
	}{
		{name: "evicts oldest at capacity", revision: issue.Revision},
		{name: "updates existing without evicting", revision: issue.Revision + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := cache.Read(t.Context(), nil, Scope{Organization: scope.Organization, Project: scope.Project}, string(issue.WorkItemID), test.revision, "policy", load); err != nil {
				t.Fatal(err)
			}
			if _, stale := cache.entries[contractKey{Item: "0"}]; stale {
				t.Fatal("oldest entry retained")
			}
			if len(cache.entries) != ReadLimit || len(cache.order) != ReadLimit || cache.oldest != 1 || cache.entries[key].Revision != test.revision {
				t.Fatalf("entries=%d order=%d oldest=%d revision=%d", len(cache.entries), len(cache.order), cache.oldest, cache.entries[key].Revision)
			}
		})
	}
}
