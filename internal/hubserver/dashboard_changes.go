package hubserver

type dashboardChange struct {
	Sequence       int64  `json:"sequence"`
	Topic          string `json:"topic"`
	EntityID       string `json:"entity_id"`
	EntityRevision int64  `json:"entity_revision"`
	Kind           string `json:"kind"`
	Status         string `json:"status"`
	RelatedID      string `json:"related_id,omitempty"`
}
