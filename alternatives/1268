package scenariotest

import (
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type Scenario struct {
	SourceFixture        string
	ImportedVersion      bool
	FinalMessage         string
	WantDisposition      *tracker.NativeDisposition
	Enrolled             bool
	RestartCompletion    bool
	LegacyReleaseRestart bool
	CompletedContinue    bool
	ContinueMessage      bool
	SummaryFailure       bool
	Feedback             string
	InitialSourceWait    bool
	Reconnect            bool
	PublicationFault     string
	Publication          string
	LargeOutput          bool
	DependencyOnly       bool
	FailedCheck          bool
	Absorbed             bool
	Delivered            string
	Hold                 bool
	LowScore             bool
	WantVerdict          string
	SSH                  bool
	Validator            string
	Name                 string
	LocalIntakeOff       bool
	Interactive          bool
	Hosted               bool
	Rework               bool
	ForeignRunner        bool
	StalePolicy          bool
	Reopened             bool
	Formal               bool
	FailDetail           bool
	FailVersion          bool
	Land                 bool
	Commit               bool
	Staged               bool
	Expired              bool
	BrokenSigning        bool
	LateConflict         bool
	AdvanceTarget        bool
	BaseMoved            bool
	StaleBase            bool
	Dirty                bool
	WantNone             bool
	WantChanged          bool
	WantState            string
	WantChanges          int
}

const DependencyMessage = "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: prj_6d4919bebd73446798e6cd807feda10e#750\n    owner: orchestrator\n    reason: prerequisite remains Backlog\n    predicate:\n      type: issue_state\n      ref: prj_6d4919bebd73446798e6cd807feda10e#750\n      states: [Done]\nhuman_action: null\n```"
const InstanceMessage = "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:tool\n    reason: effective host gate lacks a passing current-head receipt\nhuman_action: null\n```"

func DependencyDisposition() *tracker.NativeDisposition {
	return &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "prj_6d4919bebd73446798e6cd807feda10e#750", Identifier: "prj_6d4919bebd73446798e6cd807feda10e#750", Owner: workpad.BlockerOwnerOrchestrator, Reason: "prerequisite remains Backlog", RecheckInterval: "tick", Predicate: &workpad.Predicate{Type: workpad.PredicateIssueState, Ref: "prj_6d4919bebd73446798e6cd807feda10e#750", Identifier: "prj_6d4919bebd73446798e6cd807feda10e#750", States: []string{"done"}}}}}
}
func InstanceDisposition() *tracker.NativeDisposition {
	return &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "instance:tool", Owner: workpad.BlockerOwnerInstance, Reason: "effective host gate lacks a passing current-head receipt", Unverifiable: true}}}
}
