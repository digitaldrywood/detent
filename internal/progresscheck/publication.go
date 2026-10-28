package progresscheck

import "github.com/digitaldrywood/detent/internal/tracker"

type Publication struct {
	tracker.Mutation
	RequestID string   `json:"request_id"`
	PolicyID  string   `json:"policy_id"`
	Instance  string   `json:"instance"`
	Decision  Decision `json:"decision"`
}
