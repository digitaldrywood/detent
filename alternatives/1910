package eventstore

import "github.com/digitaldrywood/detent/internal/tracker"

type RunCommand struct {
	Project    tracker.ProjectID
	WorkItem   tracker.NativeWorkItemID
	LeaseID    string
	AttemptID  string
	ActorID    string
	Operation  string
	Key        string
	Sequences  []int64
	EventIDs   []string
	SessionID  int64
	Completion bool
}
