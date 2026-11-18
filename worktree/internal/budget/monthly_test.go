package budget

import (
	"math"
	"testing"
)

func TestCheckMonthly(t *testing.T) {
	t.Parallel()
	paid := CostExposure{RunnerAPI: true}
	sprite := CostExposure{SpriteInfrastructure: true}
	all := CostExposure{LunaAPI: true, RunnerAPI: true, SpriteInfrastructure: true}
	for _, test := range []struct {
		name        string
		org         MonthlyPolicy
		project     MonthlyPolicy
		orgSpend    MonthlySpend
		spend       MonthlySpend
		exposure    CostExposure
		admitted    bool
		allowed     bool
		exhausted   int
		firstScope  string
		firstBucket CostBucket
	}{
		{name: "unspecified policies", exposure: all, allowed: true},
		{name: "disabled explicit zero", org: MonthlyPolicy{RunnerMicros: new(int64)}, exposure: paid, allowed: true},
		{name: "enabled unspecified caps", org: MonthlyPolicy{Enabled: true}, exposure: all, allowed: true},
		{name: "explicit zero blocks new work", org: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64)}, exposure: paid, exhausted: 1, firstScope: "organization", firstBucket: RunnerAPI},
		{name: "org exhaustion cannot borrow project headroom", org: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64(10))}, project: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64(100))}, orgSpend: MonthlySpend{RunnerMicros: 10}, spend: MonthlySpend{RunnerMicros: 1}, exposure: paid, exhausted: 1, firstScope: "organization", firstBucket: RunnerAPI},
		{name: "project exhaustion cannot borrow org headroom", org: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64(100))}, project: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64(10))}, orgSpend: MonthlySpend{RunnerMicros: 11}, spend: MonthlySpend{RunnerMicros: 10}, exposure: paid, exhausted: 1, firstScope: "project", firstBucket: RunnerAPI},
		{name: "both exhausted org diagnostic comes first", org: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64(10))}, project: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64(10))}, orgSpend: MonthlySpend{RunnerMicros: 10}, spend: MonthlySpend{RunnerMicros: 10}, exposure: paid, exhausted: 2, firstScope: "organization", firstBucket: RunnerAPI},
		{name: "runner cannot borrow other buckets", org: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64(10)), LunaMicros: new(int64(100)), SpriteMicros: new(int64(100))}, orgSpend: MonthlySpend{RunnerMicros: 10}, exposure: paid, exhausted: 1, firstScope: "organization", firstBucket: RunnerAPI},
		{name: "luna cannot borrow runner headroom", org: MonthlyPolicy{Enabled: true, LunaMicros: new(int64(10)), RunnerMicros: new(int64(100))}, orgSpend: MonthlySpend{LunaMicros: 10}, exposure: CostExposure{LunaAPI: true}, exhausted: 1, firstScope: "organization", firstBucket: LunaAPI},
		{name: "sprite cannot borrow runner headroom", org: MonthlyPolicy{Enabled: true, SpriteMicros: new(int64(10)), RunnerMicros: new(int64(100))}, orgSpend: MonthlySpend{SpriteMicros: 10}, exposure: sprite, exhausted: 1, firstScope: "organization", firstBucket: SpriteInfrastructure},
		{name: "sprite exhaustion permits local subscription", org: MonthlyPolicy{Enabled: true, SpriteMicros: new(int64)}, exposure: CostExposure{}, allowed: true},
		{name: "sprite exhaustion permits paid local API", org: MonthlyPolicy{Enabled: true, SpriteMicros: new(int64)}, exposure: paid, allowed: true},
		{name: "luna exhaustion permits runner API", org: MonthlyPolicy{Enabled: true, LunaMicros: new(int64)}, exposure: paid, allowed: true},
		{name: "aggregate closes below individual caps", org: MonthlyPolicy{Enabled: true, TotalMicros: new(int64(30)), RunnerMicros: new(int64(20)), LunaMicros: new(int64(20)), SpriteMicros: new(int64(20))}, orgSpend: MonthlySpend{RunnerMicros: 10, LunaMicros: 10, SpriteMicros: 10}, exposure: paid, exhausted: 1, firstScope: "organization", firstBucket: Aggregate},
		{name: "aggregate counts each bucket once", org: MonthlyPolicy{Enabled: true, TotalMicros: new(int64(31))}, orgSpend: MonthlySpend{RunnerMicros: 10, LunaMicros: 10, SpriteMicros: 10}, exposure: all, allowed: true},
		{name: "project total does not add org total", org: MonthlyPolicy{Enabled: true, TotalMicros: new(int64(40))}, project: MonthlyPolicy{Enabled: true, TotalMicros: new(int64(20))}, orgSpend: MonthlySpend{RunnerMicros: 30}, spend: MonthlySpend{RunnerMicros: 10}, exposure: paid, allowed: true},
		{name: "aggregate permits free work", org: MonthlyPolicy{Enabled: true, TotalMicros: new(int64)}, exposure: CostExposure{}, allowed: true},
		{name: "default drain admits continuation", org: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64)}, exposure: paid, admitted: true, allowed: true, exhausted: 1, firstScope: "organization", firstBucket: RunnerAPI},
		{name: "project hard stop overrides org drain", org: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64)}, project: MonthlyPolicy{Enabled: true, Mode: HardStop, RunnerMicros: new(int64)}, exposure: paid, admitted: true, exhausted: 2, firstScope: "organization", firstBucket: RunnerAPI},
		{name: "org hard stop overrides project drain", org: MonthlyPolicy{Enabled: true, Mode: HardStop, RunnerMicros: new(int64)}, project: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64)}, exposure: paid, admitted: true, exhausted: 2, firstScope: "organization", firstBucket: RunnerAPI},
		{name: "sprite hard stop excludes local API", org: MonthlyPolicy{Enabled: true, Mode: HardStop, SpriteMicros: new(int64)}, exposure: paid, admitted: true, allowed: true},
		{name: "headroom is allowed", org: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64(10))}, orgSpend: MonthlySpend{RunnerMicros: 9}, exposure: paid, allowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision, err := CheckMonthly(MonthlyConstraint{Scope: "organization", Policy: test.org, Spend: test.orgSpend}, MonthlyConstraint{Scope: "project", Policy: test.project, Spend: test.spend}, test.exposure, test.admitted)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Allowed != test.allowed || len(decision.Exhausted) != test.exhausted {
				t.Fatalf("decision = %+v, want allowed=%t exhausted=%d", decision, test.allowed, test.exhausted)
			}
			if test.exhausted > 0 && (decision.Exhausted[0].Scope != test.firstScope || decision.Exhausted[0].Bucket != test.firstBucket) {
				t.Fatalf("first exhaustion = %+v, want scope=%s bucket=%s", decision.Exhausted[0], test.firstScope, test.firstBucket)
			}
		})
	}
}

func TestCheckMonthlyInvalidInputs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		policy MonthlyPolicy
		spend  MonthlySpend
	}{
		{name: "negative cap", policy: MonthlyPolicy{Enabled: true, RunnerMicros: new(int64(-1))}},
		{name: "invalid mode", policy: MonthlyPolicy{Enabled: true, Mode: "resume"}},
		{name: "negative spend", policy: MonthlyPolicy{Enabled: true, TotalMicros: new(int64(10))}, spend: MonthlySpend{RunnerMicros: -1}},
		{name: "aggregate overflow first addition", policy: MonthlyPolicy{Enabled: true, TotalMicros: new(int64(10))}, spend: MonthlySpend{RunnerMicros: math.MaxInt64, LunaMicros: 1}},
		{name: "aggregate overflow second addition", policy: MonthlyPolicy{Enabled: true, TotalMicros: new(int64(10))}, spend: MonthlySpend{RunnerMicros: math.MaxInt64 - 1, LunaMicros: 1, SpriteMicros: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := CheckMonthly(MonthlyConstraint{Policy: test.policy, Spend: test.spend}, MonthlyConstraint{}, CostExposure{RunnerAPI: true}, false)
			if err == nil {
				t.Fatal("invalid budget inputs were accepted")
			}
		})
	}
}
