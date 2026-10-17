package hubserver

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type reportsQualityInputsKey struct{}

type reportsQualityRead struct {
	current, prior operatortool.AnalyticsWindow
	inputs         *analyticsQualityInputs
	read           bool
}

func withReportsQualityInputs(ctx context.Context, w operatortool.AnalyticsWindow) context.Context {
	prior := operatortool.AnalyticsWindow{From: w.From.Add(-w.To.Sub(w.From)), To: w.From, Bucket: w.Bucket}
	return context.WithValue(ctx, reportsQualityInputsKey{}, &reportsQualityRead{current: w, prior: prior})
}

func reportsQualityInputs(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow) (analyticsQualityInputs, error) {
	read, ok := ctx.Value(reportsQualityInputsKey{}).(*reportsQualityRead)
	if !ok || w != read.current && w != read.prior {
		return loadAnalyticsQualityInputs(ctx, q, scope, w)
	}
	if !read.read {
		combined := operatortool.AnalyticsWindow{From: read.prior.From, To: read.current.To, Bucket: w.Bucket}
		inputs, err := loadAnalyticsQualityInputs(context.WithValue(ctx, analyticsPagingKey{}, nil), q, scope, combined)
		if err != nil {
			return analyticsQualityInputs{}, err
		}
		read.read = true
		if !inputs.partial {
			read.inputs = &inputs
		}
	}
	if read.inputs == nil {
		return loadAnalyticsQualityInputs(ctx, q, scope, w)
	}
	return qualityInputsInWindow(*read.inputs, w), nil
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
