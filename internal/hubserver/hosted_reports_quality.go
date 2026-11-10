package hubserver

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func reportsQualityInputs(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow) (analyticsQualityInputs, error) {
	return loadAnalyticsQualityInputs(ctx, q, scope, w)
}

func qualityInputsInWindow(in analyticsQualityInputs, w operatortool.AnalyticsWindow) analyticsQualityInputs {
	out := analyticsQualityInputs{}
	reopened := make(map[string]bool)
	versions := make(map[string]bool)
	for _, event := range in.events {
		if event.At.Before(w.From) || !event.At.Before(w.To) {
			continue
		}
		out.events = append(out.events, event)
		if event.Kind == "workflow.transitioned" && strings.EqualFold(event.Data.FromState, "Done") {
			reopened[event.Item] = true
		}
	}
	for _, discussion := range in.discussions {
		if discussion.Escape == nil || discussion.Escape.ObservedAt.Before(w.From) || !discussion.Escape.ObservedAt.Before(w.To) {
			continue
		}
		out.discussions = append(out.discussions, discussion)
		versions[discussion.ChangeID+"/"+discussion.VersionID] = true
	}
	for _, landing := range in.landings {
		if !landing.Landing.LandedAt.Before(w.To) {
			continue
		}
		if !landing.Landing.LandedAt.Before(w.From) || reopened[string(landing.WorkItemID)] || versions[landing.ChangeID+"/"+landing.Landing.VersionID] {
			out.landings = append(out.landings, landing)
		}
	}
	return out
}
