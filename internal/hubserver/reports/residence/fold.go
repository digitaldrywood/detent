package residence

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Issue struct {
	Id             string                   `json:"-"`
	Created        time.Time                `json:"-"`
	Initial        string                   `json:"-"`
	Events         []Event                  `json:"-"`
	Partial        bool                     `json:"-"`
	Checkpoint     *reportstore.HistoryBase `json:"-"`
	OmittedPartial bool                     `json:"-"`
}

type Event struct {
	At   time.Time                 `json:"-"`
	Kind string                    `json:"-"`
	Data tracker.CollaborationData `json:"-"`
}

type Span struct {
	Lane    string    `json:"-"`
	From    time.Time `json:"-"`
	To      time.Time `json:"-"`
	QueueTo time.Time `json:"-"`
}

type Fold struct {
	Issue   Issue `json:"-"`
	states  map[string]tracker.NativeState
	To      time.Time `json:"-"`
	queueTo time.Time
	last    time.Time
	Started *time.Time `json:"-"`
	State   string     `json:"-"`
	Entered time.Time  `json:"-"`
}

type Delta struct {
	Span    Span
	HasSpan bool
	Done    bool `json:"-"`
	Rework  bool
}

func NewFold(issue Issue, states map[string]tracker.NativeState, to time.Time) Fold {
	if base := issue.Checkpoint; base != nil {
		fold := Fold{Issue: issue, states: states, To: to, queueTo: to, last: base.Last, State: base.State, Entered: base.Entered}
		fold.Issue.Partial = issue.Partial || issue.OmittedPartial || base.Partial
		if base.QueueCut != nil {
			fold.queueTo = *base.QueueCut
		}
		if base.Started != nil {
			at := *base.Started
			fold.Started = &at
		}
		return fold
	}
	fold := Fold{Issue: issue, states: states, To: to, queueTo: to, last: issue.Created, State: issue.Initial, Entered: issue.Created}
	if issue.Initial == "" || issue.OmittedPartial {
		fold.Issue.Partial = true
	}
	if states[issue.Initial].Dispatchable && Group(issue.Initial, states) == "system" {
		at := issue.Created
		fold.Started = &at
	}
	return fold
}

func (fold *Fold) Append(event Event) Delta {
	delta := Delta{}
	if event.At.Before(fold.last) {
		fold.Issue.Partial = true
		return delta
	}
	fold.last = event.At
	if event.Kind == "scheduler.decision" {
		if fold.State == "Rework" && fold.queueTo.After(event.At) {
			fold.queueTo = event.At
		}
		return delta
	}
	if event.Data.FromState != fold.State || event.Data.ToState == "" {
		fold.Issue.Partial = true
	} else if event.Data.ToState != fold.State && fold.Started != nil {
		delta.Span = Span{Lane: fold.State, From: hubapi.MaxTime(fold.Entered, *fold.Started), To: event.At, QueueTo: hubapi.MinTime(fold.queueTo, event.At)}
		delta.HasSpan = true
	}
	if event.Data.ToState == "" || event.Data.ToState == fold.State {
		return delta
	}
	fold.State, fold.Entered = event.Data.ToState, event.At
	fold.queueTo = fold.To
	if fold.Started == nil && fold.states[fold.State].Dispatchable && Group(fold.State, fold.states) == "system" {
		at := event.At
		fold.Started = &at
	}
	delta.Done = fold.State == "Done"
	delta.Rework = fold.State == "Rework"
	return delta
}

func (fold *Fold) Finish() (Span, bool) {
	if fold.Started == nil || fold.Issue.Partial {
		return Span{}, false
	}
	return Span{Lane: fold.State, From: hubapi.MaxTime(fold.Entered, *fold.Started), To: fold.To, QueueTo: fold.queueTo}, true
}

func Group(lane string, states map[string]tracker.NativeState) string {
	if lane == "Backlog" || lane == "Triage" || lane == "Done" || lane == "Cancelled" || states[lane].Terminal {
		return "excluded"
	}
	if lane == "Blocked" || lane == "Human Review" {
		return "held"
	}
	return "system"
}
