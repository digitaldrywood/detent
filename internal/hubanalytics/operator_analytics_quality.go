package hubanalytics

import (
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type QualityCounts struct {
	WorkedItems     int            `json:"worked_items"`
	ReworkedItems   int            `json:"reworked_items"`
	LandedVersions  int            `json:"landed_versions"`
	EscapedVersions int            `json:"escaped_versions"`
	Escapes         int            `json:"escapes"`
	Infrastructure  int            `json:"infrastructure"`
	InfraReturns    int            `json:"infrastructure_returns"`
	Pending         int            `json:"pending_classification"`
	ReworkPercent   *float64       `json:"rework_percent"`
	EscapePercent   *float64       `json:"escape_percent"`
	Causes          map[string]int `json:"causes"`
}

type QualityBucket struct {
	QualityCounts
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type QualityEscape struct {
	tracker.QualityEscape
	ChangeID                string        `json:"change_id"`
	VersionID               string        `json:"version_id"`
	WorkItemID              string        `json:"work_item_id,omitempty"`
	MergeSHA                string        `json:"merge_sha"`
	LandedAt                time.Time     `json:"landed_at"`
	DiscussionID            string        `json:"discussion_id,omitempty"`
	Actor                   tracker.Actor `json:"actor"`
	Attribution             string        `json:"attribution"`
	LandingContextAvailable bool          `json:"landing_context_available"`
}

type Quality struct {
	QualityCounts
	Source      string                               `json:"source"`
	Coverage    string                               `json:"coverage"`
	Partial     bool                                 `json:"partial"`
	Unavailable []string                             `json:"unavailable"`
	Buckets     []QualityBucket                      `json:"buckets"`
	Occurrences operatortool.ReadPage[QualityEscape] `json:"occurrences"`
	Returns     []QualityReturn                      `json:"-"`
}

type QualityReturn struct {
	Item  string    `json:"-"`
	At    time.Time `json:"-"`
	Class Class     `json:"-"`
}
