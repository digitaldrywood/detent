package runnerupdate

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestApplyUrgentRunnerRoutingFollowHubReceipt(t *testing.T) {
	request := runnerauth.UpdateRequest{ID: "follow-0.129.3", Service: "detent", RequestedAt: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Version: "0.129.3", Release: true, FollowHub: true}
	earlier := request
	earlier.ID = "follow-0.129.3-earlier"
	tests := []struct {
		name    string
		receipt *runnerauth.UpdateReceipt
		want    string
	}{
		{name: "no receipt stays active", want: "active"},
		{name: "refused receipt stays active", receipt: &runnerauth.UpdateReceipt{Request: request, Status: "refused", FailureReason: "update is unavailable for unknown installs"}, want: "active"},
		{name: "uncertain receipt with failure stays active", receipt: &runnerauth.UpdateReceipt{Request: request, Status: "uncertain", FailureReason: "download failed"}, want: "active"},
		{name: "uncertain receipt without failure stays active", receipt: &runnerauth.UpdateReceipt{Request: request, Status: "uncertain"}, want: "active"},
		{name: "applied receipt stays active", receipt: &runnerauth.UpdateReceipt{Request: request, Status: "applied"}, want: "active"},
		{name: "refused receipt for an earlier request stays active", receipt: &runnerauth.UpdateReceipt{Request: earlier, Status: "refused", FailureReason: "update is unavailable for unknown installs"}, want: "active"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := runnerauth.Runner{Routing: runnerauth.Routing{State: "active", UpdateRequest: &request}, Update: &runnerauth.UpdateObservation{Running: runnerauth.BuildEvidence{Version: "0.129.2"}, Receipt: test.receipt}}
			ApplyUrgentRouting(&r, UrgentUpdate{}, "0.129.2")
			if r.State != test.want {
				t.Fatalf("state = %q, want %q", r.State, test.want)
			}
		})
	}
}
