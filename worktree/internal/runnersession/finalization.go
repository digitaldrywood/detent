package runnersession

import (
	"errors"
	"reflect"
)

var ErrFinalizationIncomplete = errors.New("session finalization is incomplete")

func ApplyFinalization(snapshot *Snapshot, request Mutation) (bool, error) {
	value := request.Finalization
	if value == nil {
		if snapshot.Finalization != nil && (request.Finish != nil || request.Usage != nil || request.Phase != nil) {
			return false, errors.New("immutable session finalization is required")
		}
		return false, nil
	}
	if value.SessionID != request.SessionID || value.SessionID != snapshot.ID || value.SessionID <= 0 || value.Usage.SessionID != value.SessionID || value.Usage.IssueID != snapshot.Start.IssueID || value.Usage.ProjectID != snapshot.Start.ProjectID || !value.Usage.StartedAt.Equal(snapshot.Start.StartedAt) || value.Finish.CompletedAt.IsZero() || !value.Finish.CompletedAt.Equal(value.Usage.FinishedAt) || value.Finish.FinalState != value.Usage.Outcome {
		return false, errors.New("session finalization differs from its immutable identity")
	}
	if phase := value.Phase; phase == nil || phase.SessionID != value.SessionID || phase.IssueID != value.Usage.IssueID || phase.ProjectID != value.Usage.ProjectID || !phase.StartedAt.Equal(value.Usage.StartedAt) || !phase.FinishedAt.Equal(value.Finish.CompletedAt) || phase.PhaseType != "agent_session" || phase.PhaseName != "agent_active" || phase.Status != value.Finish.FinalState {
		return false, errors.New("session finalization phase differs from its immutable identity")
	}
	stage := "finish"
	if request.Usage != nil {
		stage = "usage"
	}
	if request.Phase != nil {
		stage = "phase"
	}
	key, err := value.Key(stage)
	if err != nil || request.Key != key {
		return false, errors.New("session finalization mutation key differs from its immutable identity")
	}
	if snapshot.Finalization == nil {
		if snapshot.Finish != nil && !reflect.DeepEqual(*snapshot.Finish, value.Finish) {
			return false, errors.New("session finalization changes recorded completion")
		}
		snapshot.Finalization = &Finalization{Input: *value}
	} else if !snapshot.Finalization.Input.Equal(*value) {
		return false, errors.New("session finalization inputs changed")
	}
	state := snapshot.Finalization
	switch {
	case request.Finish != nil && reflect.DeepEqual(*request.Finish, value.Finish):
		if state.Finished {
			return true, nil
		}
		state.Finished = true
	case request.Usage != nil && reflect.DeepEqual(*request.Usage, value.Usage):
		if !state.Finished {
			return false, errors.New("session usage requires completed finish")
		}
		if state.UsageRecorded {
			return true, nil
		}
		state.UsageRecorded = true
	case request.Phase != nil && value.Phase != nil && reflect.DeepEqual(*request.Phase, *value.Phase):
		if !state.UsageRecorded {
			return false, errors.New("session phase requires recorded usage")
		}
		if state.PhaseRecorded {
			return true, nil
		}
		state.PhaseRecorded = true
	default:
		return false, errors.New("session mutation differs from its finalization")
	}
	return false, nil
}

func VerifyLegacyFinalization(current Snapshot, entries []Mutation) error {
	if current.Finish == nil || current.Revision < 4 || len(entries) < 3 || len(entries) > HistoryLimit {
		return ErrFinalizationIncomplete
	}
	var finish, usage, phase bool
	for _, entry := range entries {
		if entry.SessionID != current.ID {
			return ErrFinalizationIncomplete
		}
		count := 0
		for _, present := range []bool{entry.Finish != nil, entry.Usage != nil, entry.Phase != nil} {
			if present {
				count++
			}
		}
		if count > 1 {
			return ErrFinalizationIncomplete
		}
		if entry.Finish != nil && !finish {
			if !reflect.DeepEqual(entry.Finish, current.Finish) {
				return ErrFinalizationIncomplete
			}
			finish = true
		}
		if entry.Usage != nil && !usage {
			value := entry.Usage
			if value.SessionID != current.ID || value.IssueID != current.Start.IssueID || value.ProjectID != current.Start.ProjectID || !value.StartedAt.Equal(current.Start.StartedAt) || !value.FinishedAt.Equal(current.Finish.CompletedAt) || value.Outcome != current.Finish.FinalState {
				return ErrFinalizationIncomplete
			}
			usage = true
		}
		if entry.Phase != nil && !phase {
			value := entry.Phase
			if value.SessionID != current.ID || value.IssueID != current.Start.IssueID || value.ProjectID != current.Start.ProjectID || value.PhaseType != "agent_session" || value.PhaseName != "agent_active" || !value.StartedAt.Equal(current.Start.StartedAt) || !value.FinishedAt.Equal(current.Finish.CompletedAt) || value.Status != current.Finish.FinalState {
				return ErrFinalizationIncomplete
			}
			phase = true
		}
		if finish && usage && phase {
			return nil
		}
	}
	return ErrFinalizationIncomplete
}
