package reports

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func qualityInputsInWindow(in AnalyticsQualityInputs, w operatortool.AnalyticsWindow) AnalyticsQualityInputs {
	out := AnalyticsQualityInputs{}
	reopened := make(map[string]bool)
	versions := make(map[string]bool)
	for _, event := range in.Events {
		if event.At.Before(w.From) || !event.At.Before(w.To) {
			continue
		}
		out.Events = append(out.Events, event)
		if event.Kind == "workflow.transitioned" && strings.EqualFold(event.Data.FromState, "Done") {
			reopened[event.Item] = true
		}
	}
	for _, discussion := range in.Discussions {
		if discussion.Escape == nil || discussion.Escape.ObservedAt.Before(w.From) || !discussion.Escape.ObservedAt.Before(w.To) {
			continue
		}
		out.Discussions = append(out.Discussions, discussion)
		versions[discussion.ChangeID+"/"+discussion.VersionID] = true
	}
	for _, landing := range in.Landings {
		if !landing.Landing.LandedAt.Before(w.To) {
			continue
		}
		if !landing.Landing.LandedAt.Before(w.From) || reopened[string(landing.WorkItemID)] || versions[landing.ChangeID+"/"+landing.Landing.VersionID] {
			out.Landings = append(out.Landings, landing)
		}
	}
	return out
}
