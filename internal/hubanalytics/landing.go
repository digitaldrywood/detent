package hubanalytics

import "github.com/digitaldrywood/detent/internal/tracker"

type Landing struct {
	ChangeID   string                   `json:"change_id"`
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
	Landing    tracker.ChangeLanding    `json:"landing"`
}
