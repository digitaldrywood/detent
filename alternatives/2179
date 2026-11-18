package hubscope

import (
	"github.com/digitaldrywood/detent/internal/gate"
)

const ScheduledImportTrustChanged = "Scheduled CI import authorization changed before creation or Todo is not dispatchable"

func (v *NativeScheduledRepair) Trusted() bool {
	return v != nil && v.ImportReason == "" && v.PolicyID != ""
}

func (v *NativeScheduledRepair) TrustedLinkedImport() bool {
	return v.Trusted() && v.ImportURL != ""
}

func (v *NativeScheduledRepair) DowngradeTrust() {
	v.PolicyID = ""
	if v.ImportURL != "" {
		v.ImportReason = ScheduledImportTrustChanged
	}
}

type NativeScheduledRepair struct {
	PolicyID     string                 `json:"-"`
	Evidence     gate.ScheduledEvidence `json:"-"`
	ImportURL    string                 `json:"-"`
	ImportBody   string                 `json:"-"`
	ImportReason string                 `json:"-"`
}
