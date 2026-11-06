package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"time"
)

func applyPostgresReportQuality(out *hostedReportsReport, history []reportstore.ProjectHistory, landings []reportstore.LandingFact, escapes []reportstore.EscapeFact, facts []reportstore.OverviewFact, w, prior operatortool.AnalyticsWindow) {
	if w.Bucket <= 0 {
		w.Bucket = time.Hour
		if w.To.Sub(w.From) >= 6*24*time.Hour {
			w.Bucket = 24 * time.Hour
		}
	}
	prior.Bucket = w.Bucket
	landed := []nativeAnalyticsLanding{}
	for _, fact := range landings {
		landed = append(landed, nativeAnalyticsLanding{ChangeID: fact.ChangeID, WorkItemID: fact.WorkItemID, Landing: tracker.ChangeLanding{VersionID: fact.VersionID, HeadSHA: fact.HeadSHA, MergeSHA: fact.MergeSHA, BaseRef: fact.BaseRef, LandedAt: fact.At}})
	}
	events := []qualityHistoryEvent{}
	for _, project := range history {
		terminal := map[string]bool{}
		for _, state := range project.States {
			terminal[state.Name] = state.Terminal
		}
		for _, issue := range project.Issues {
			for _, event := range issue.Events {
				if event.Type == "workflow.transitioned" {
					events = append(events, qualityHistoryEvent{ID: event.ID, Item: string(issue.WorkItemID), Kind: event.Type, VersionID: event.VersionID, ToTerminal: terminal[event.ToState], At: event.At, Data: tracker.CollaborationData{FromState: event.FromState, ToState: event.ToState, ReasonDetail: event.Class.Name}})
				}
			}
		}
	}
	for _, fact := range facts {
		if fact.Worked {
			events = append(events, qualityHistoryEvent{Item: string(fact.WorkItemID), Kind: "worked", At: w.From})
		}
		if fact.PreviousWorked {
			events = append(events, qualityHistoryEvent{Item: string(fact.WorkItemID), Kind: "worked", At: prior.From})
		}
	}
	discussions := []qualityDiscussion{}
	for _, fact := range escapes {
		escape := tracker.QualityEscape{OccurrenceID: fact.OccurrenceID, Kind: fact.Kind, Cause: fact.Cause, ObservedAt: fact.ObservedAt, InstanceID: fact.InstanceID, HumanOverride: fact.HumanOverride}
		discussions = append(discussions, qualityDiscussion{ChangeID: fact.ChangeID, ChangeDiscussion: tracker.ChangeDiscussion{VersionID: fact.VersionID, CreatedAt: fact.CreatedAt, Actor: tracker.Actor{Kind: fact.ActorKind, PrincipalID: fact.ActorID}, Escape: &escape}})
	}
	quality := summarizeAnalyticsQuality(landed, events, discussions, w)
	quality.Source = "postgres_typed_landing_workflow_and_escape_facts"
	quality.Coverage = "bounded typed scalar classifications; evidence bodies remain in immutable object storage"
	out.Analytics.Quality = quality
	previous := summarizeAnalyticsQuality(landed, events, discussions, prior)
	out.Prior.ReworkPercent = previous.ReworkPercent
	out.Prior.InfrastructureReturns = &previous.InfraReturns
	reworks := []postgresReportClassEvent{}
	for _, event := range quality.returns {
		reworks = append(reworks, postgresReportClassEvent{class: event.class, item: event.item, at: event.at})
	}
	out.Rework = postgresReportClasses(reworks)
	for i := range out.Series {
		out.Series[i].ReworkClasses = map[string]int{}
	}
	index := reportsSeriesIndex(reportsSeriesSpans(w))
	for _, event := range reworks {
		if i := index(event.at); i >= 0 {
			out.Series[i].ReworkClasses[event.class.Name]++
		}
	}
	out.Analytics.Unavailable = postgresReportRemoveUnavailable(out.Analytics.Unavailable, "escape_classification", "infrastructure_return_classification")
}
