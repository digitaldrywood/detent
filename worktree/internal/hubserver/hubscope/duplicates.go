package hubscope

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

type PossibleDuplicate struct {
	Revision   tracker.Revision         `json:"-"`
	Terminal   bool                     `json:"-"`
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
	Identifier string                   `json:"identifier"`
	Number     int                      `json:"number"`
	Title      string                   `json:"title"`
	State      string                   `json:"state"`
	WebURL     string                   `json:"web_url"`
	Why        string                   `json:"why"`
}
