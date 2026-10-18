package conversation

import (
	"encoding/json"
	"time"
)

// Resume values report what a bound runner continued from (decisions
// section 10.4). The empty value means the runner started fresh.
const (
	ResumeThread     = "thread"
	ResumeTranscript = "transcript"
)

// ThreadOrigin values name the kind of turn that produced a conversation's
// provider thread (decisions section 9.3). A coordinator thread was started
// with the coordinator instructions and its permission set: no checkout, no
// edits, no issue state. A worker thread was started by an attempt on the
// linked issue. Only a turn of the same kind may resume a thread, because the
// thread carries the restrictions of the turn that opened it. The empty value
// means the origin is unknown and nothing resumes it.
const (
	ThreadOriginCoordinator = "coordinator"
	ThreadOriginWorker      = "worker"
)

// ThreadOriginFor reports the origin a turn records for the thread it starts.
func ThreadOriginFor(coordinator bool) string {
	if coordinator {
		return ThreadOriginCoordinator
	}
	return ThreadOriginWorker
}

// Execution is the runner state reported on a conversation resource.
type Execution struct {
	Status ExecutionStatus
	Owner  Owner
	// Resume is ResumeThread when the bound runner continued its own
	// provider thread, ResumeTranscript when it was handed a transcript
	// instead, and empty when neither applies.
	Resume       string
	Capabilities Capabilities
	Error        string
	// Settled reports that the bound worker's unbind already recorded the
	// outcome, so a repeated unbind changes nothing. Lease loss and a
	// terminal turn do not settle: the worker's own verdict still wins.
	Settled   bool
	UpdatedAt time.Time
}

type executionWire struct {
	Status       ExecutionStatus `json:"status"`
	AttemptID    string          `json:"attempt_id"`
	RunID        string          `json:"run_id"`
	LeaseID      string          `json:"lease_id"`
	FencingToken int64           `json:"fencing_token"`
	RunnerID     string          `json:"runner_id"`
	MachineID    string          `json:"machine_id"`
	ThreadID     string          `json:"thread_id"`
	TurnID       string          `json:"turn_id"`
	Resume       string          `json:"resume"`
	Capabilities Capabilities    `json:"capabilities"`
	Settled      bool            `json:"settled,omitempty"`
	Error        *string         `json:"error"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// MarshalJSON renders the execution object with the owner tuple flattened,
// matching the contract shape.
func (e Execution) MarshalJSON() ([]byte, error) {
	return marshalJSON(executionWire{
		Status:       e.Status,
		AttemptID:    e.Owner.AttemptID,
		RunID:        e.Owner.RunID,
		LeaseID:      e.Owner.LeaseID,
		FencingToken: e.Owner.FencingToken,
		RunnerID:     e.Owner.RunnerID,
		MachineID:    e.Owner.MachineID,
		ThreadID:     e.Owner.ThreadID,
		TurnID:       e.Owner.TurnID,
		Resume:       e.Resume,
		Capabilities: e.Capabilities,
		Settled:      e.Settled,
		Error:        nullable(e.Error),
		UpdatedAt:    e.UpdatedAt,
	})
}

// UnmarshalJSON reads the flattened contract shape.
func (e *Execution) UnmarshalJSON(data []byte) error {
	var wire executionWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*e = Execution{
		Status: wire.Status,
		Owner: Owner{
			AttemptID:    wire.AttemptID,
			RunID:        wire.RunID,
			LeaseID:      wire.LeaseID,
			FencingToken: wire.FencingToken,
			RunnerID:     wire.RunnerID,
			MachineID:    wire.MachineID,
			ThreadID:     wire.ThreadID,
			TurnID:       wire.TurnID,
		},
		Resume:       wire.Resume,
		Capabilities: wire.Capabilities,
		Settled:      wire.Settled,
		UpdatedAt:    wire.UpdatedAt,
	}
	if wire.Error != nil {
		e.Error = *wire.Error
	}
	return nil
}

func marshalJSON(value any) ([]byte, error) { return json.Marshal(value) }

// NormalizeExecution maps in-flight execution states to unknown after a hub
// restart. Idle, waiting and terminal states are unchanged.
func NormalizeExecution(status ExecutionStatus) ExecutionStatus {
	switch status {
	case ExecutionStarting, ExecutionRunning, ExecutionWaitingInput, ExecutionInterrupting:
		return ExecutionUnknown
	default:
		return status
	}
}

// NormalizeExecutionValue applies NormalizeExecution to a whole execution
// object, clearing capabilities when the runner state became unknown.
func NormalizeExecutionValue(execution Execution) Execution {
	normalized := NormalizeExecution(execution.Status)
	if normalized == execution.Status {
		return execution
	}
	execution.Status = normalized
	execution.Capabilities = Capabilities{}
	execution.Resume = ""
	return execution
}

// NormalizeDelivery maps deliveries that were in flight to unknown after a
// hub restart. Queued controls stay queued and are never replayed; a control
// that had reached the worker cannot be re-established, so it becomes
// unknown for the user to retry explicitly (decisions section 10.3).
func NormalizeDelivery(delivery Delivery) Delivery {
	switch delivery {
	case DeliverySending, DeliverySent, DeliveryResponding:
		return DeliveryUnknown
	default:
		return delivery
	}
}

// NormalizeQuestion expires pending questions and marks questions that were
// being sent as unknown after a hub restart.
func NormalizeQuestion(status QuestionStatus) QuestionStatus {
	switch status {
	case QuestionPending:
		return QuestionExpired
	case QuestionSending:
		return QuestionUnknown
	default:
		return status
	}
}
