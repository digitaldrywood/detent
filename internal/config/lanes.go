package config

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

const (
	LaneActive        = "active"
	LaneHolding       = "holding"
	LaneTerminal      = "terminal"
	LegacyLaneWarning = "tracker.observed_states, tracker.active_states and tracker.terminal_states must be migrated to tracker.lanes with detent config migrate; the next release will reject the old keys"
)

type Lane struct {
	Name string `yaml:"name" json:"name"`
	Role string `yaml:"role" json:"role"`
}

func (t *Tracker) UnmarshalYAML(node *yaml.Node) error {
	type plain Tracker
	value := plain(*t)
	var fields map[string]yaml.Node
	if err := node.Decode(&fields); err != nil {
		return err
	}
	_, lanesSet := fields["lanes"]
	legacy := false
	for _, field := range []struct {
		key    string
		states *[]string
	}{
		{"observed_states", &value.ObservedStates},
		{"active_states", &value.ActiveStates},
		{"terminal_states", &value.TerminalStates},
	} {
		if fieldNode, exists := fields[field.key]; exists {
			legacy = true
			if lanesSet {
				return errors.New("tracker.lanes cannot be combined with observed_states, active_states or terminal_states")
			}
			if err := fieldNode.Decode(field.states); err != nil {
				return fmt.Errorf("tracker.%s: %w", field.key, err)
			}
		}
	}
	if err := node.Decode(&value); err != nil {
		return err
	}
	*t = Tracker(value)
	t.legacyStates = legacy
	if lanesSet {
		if t.Lanes == nil {
			return errors.New("tracker.lanes must be a list")
		}
		t.ActiveStates, t.ObservedStates, t.TerminalStates = nil, nil, nil
		for _, lane := range t.Lanes {
			switch lane.Role {
			case LaneActive:
				t.ActiveStates = append(t.ActiveStates, lane.Name)
			case LaneHolding:
				t.ObservedStates = append(t.ObservedStates, lane.Name)
			case LaneTerminal:
				t.TerminalStates = append(t.TerminalStates, lane.Name)
			}
		}
	} else if legacy {
		t.Lanes = nil
		t.Lanes = t.WorkflowLanes()
	}
	return nil
}

func (t Tracker) MarshalYAML() (any, error) {
	type plain Tracker
	value := plain(t)
	value.Lanes = t.WorkflowLanes()
	return value, nil
}

func (t Tracker) WorkflowLanes() []Lane {
	if t.Lanes != nil {
		return t.Lanes
	}
	lanes := make([]Lane, 0, len(t.ObservedStates)+len(t.ActiveStates)+len(t.TerminalStates))
	seen := make(map[string]bool)
	for _, states := range [][]string{t.ObservedStates, t.ActiveStates, t.TerminalStates} {
		for _, name := range states {
			name = cleanKanbanPolicyState(name)
			key := kanbanPolicyStateKey(name)
			if name == "" || seen[key] {
				continue
			}
			seen[key] = true
			role := LaneHolding
			if stateListContains(t.TerminalStates, name) {
				role = LaneTerminal
			} else if stateListContains(t.ActiveStates, name) {
				role = LaneActive
			}
			lanes = append(lanes, Lane{Name: name, Role: role})
		}
	}
	return lanes
}

func validateLanes(lanes []Lane) error {
	seen := make(map[string]bool)
	for index, lane := range lanes {
		key := kanbanPolicyStateKey(lane.Name)
		if key == "" {
			return fmt.Errorf("tracker.lanes[%d].name must not be blank", index)
		}
		if seen[key] {
			return errors.New("tracker.lanes state names must be unique")
		}
		seen[key] = true
		switch lane.Role {
		case LaneActive, LaneHolding, LaneTerminal:
		default:
			return fmt.Errorf("tracker.lanes[%d].role must be active, holding or terminal", index)
		}
	}
	return nil
}

func (t Tracker) roleField(role string) string {
	if t.Lanes != nil && !t.legacyStates {
		return "tracker.lanes with role " + role
	}
	switch role {
	case LaneActive:
		return "tracker.active_states"
	case LaneTerminal:
		return "tracker.terminal_states"
	default:
		return "tracker.observed_states"
	}
}
