package hostpressure

import "time"

type MemoryPressure struct {
	Supported    bool             `json:"supported"`
	Some         PressureAverages `json:"some"`
	Full         PressureAverages `json:"full"`
	SomeAvg60Max float64          `json:"some_avg60_max"`
	DispatchHeld bool             `json:"dispatch_held"`
	ObservedAt   time.Time        `json:"observed_at,omitzero"`
	LastError    string           `json:"last_error,omitempty"`
}

type IOPressure struct {
	Supported                    bool             `json:"supported"`
	Some                         PressureAverages `json:"some"`
	Full                         PressureAverages `json:"full"`
	FullAvg10Max                 float64          `json:"full_avg10_max"`
	DegradedMaxConcurrentAgents  int              `json:"degraded_max_concurrent_agents"`
	EffectiveMaxConcurrentAgents int              `json:"effective_max_concurrent_agents"`
	CapacityConstrained          bool             `json:"capacity_constrained"`
	DispatchHeld                 bool             `json:"dispatch_held"`
	ConstrainedSince             time.Time        `json:"constrained_since,omitzero"`
	ConstrainedForMS             int64            `json:"constrained_for_ms"`
	ObservedAt                   time.Time        `json:"observed_at,omitzero"`
	LastError                    string           `json:"last_error,omitempty"`
}

type CPUPressure struct {
	Supported                    bool             `json:"supported"`
	Some                         PressureAverages `json:"some"`
	Full                         PressureAverages `json:"full"`
	SomeAvg10Max                 float64          `json:"some_avg10_max"`
	DegradedMaxConcurrentAgents  int              `json:"degraded_max_concurrent_agents"`
	EffectiveMaxConcurrentAgents int              `json:"effective_max_concurrent_agents"`
	CapacityConstrained          bool             `json:"capacity_constrained"`
	DispatchHeld                 bool             `json:"dispatch_held"`
	ConstrainedSince             time.Time        `json:"constrained_since,omitzero"`
	ConstrainedForMS             int64            `json:"constrained_for_ms"`
	ObservedAt                   time.Time        `json:"observed_at,omitzero"`
	LastError                    string           `json:"last_error,omitempty"`
}

type PressureAverages struct {
	Avg10  float64 `json:"avg10"`
	Avg60  float64 `json:"avg60"`
	Avg300 float64 `json:"avg300"`
	Total  uint64  `json:"total"`
}
