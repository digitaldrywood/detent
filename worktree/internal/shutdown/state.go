package shutdown

type Phase string

const (
	PhaseRunning  Phase = "running"
	PhaseDraining Phase = "draining"
	PhaseComplete Phase = "complete"
)

type Result string

const (
	ResultNone     Result = ""
	ResultGraceful Result = "graceful"
	ResultForced   Result = "forced"
	ResultTimeout  Result = "timeout"
)

type Event string

const (
	EventDrainRequested Event = "drain_requested"
	EventForceRequested Event = "force_requested"
	EventDrained        Event = "drained"
	EventDrainTimedOut  Event = "drain_timed_out"
)

type Machine struct {
	Phase  Phase
	Result Result
}

func NewMachine() Machine {
	return Machine{Phase: PhaseRunning}
}

func (m Machine) Apply(event Event) Machine {
	if m.Phase == PhaseComplete {
		return m
	}

	switch event {
	case EventDrainRequested:
		m.Phase = PhaseDraining
		m.Result = ResultNone
	case EventDrained:
		m.Phase = PhaseComplete
		m.Result = ResultGraceful
	case EventForceRequested:
		m.Phase = PhaseComplete
		m.Result = ResultForced
	case EventDrainTimedOut:
		m.Phase = PhaseComplete
		m.Result = ResultTimeout
	}
	return m
}
