package workpad

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

type DecisionRequest struct {
	AttemptID         string           `json:"attempt_id,omitempty" yaml:"-"`
	Summary           string           `json:"summary" yaml:"summary"`
	Question          string           `json:"question" yaml:"question"`
	Options           []DecisionOption `json:"options" yaml:"options"`
	CustomAllowed     bool             `json:"custom_allowed" yaml:"custom_allowed"`
	CustomReturnState string           `json:"custom_return_state" yaml:"custom_return_state"`
}

type DecisionOption struct {
	Label       string `json:"label" yaml:"label"`
	Consequence string `json:"consequence" yaml:"consequence"`
	Recommended bool   `json:"recommended" yaml:"recommended"`
	ReturnState string `json:"return_state" yaml:"return_state"`
}

func (d *DecisionRequest) Validate() error {
	if d == nil {
		return errors.New("blocked completion with human_action requires decision_request")
	}
	for _, field := range []struct {
		name, value string
		limit       int
	}{
		{"summary", d.Summary, 1024}, {"question", d.Question, 2048},
	} {
		if strings.TrimSpace(field.value) == "" || !utf8.ValidString(field.value) || len(field.value) > field.limit || strings.ContainsAny(field.value, "\r\n") {
			return fmt.Errorf("decision_request.%s requires bounded single-line text", field.name)
		}
	}
	if len(d.Options) < 2 || len(d.Options) > 4 {
		return errors.New("decision_request requires two to four options")
	}
	if !d.CustomAllowed || !decisionReturnState(d.CustomReturnState) {
		return errors.New("decision_request requires custom_allowed and custom_return_state Todo or Rework")
	}
	recommended := 0
	labels := map[string]bool{}
	for i, option := range d.Options {
		label := strings.TrimSpace(option.Label)
		if label == "" || len(label) > 256 || !utf8.ValidString(label) || strings.ContainsAny(label, "\r\n") || labels[label] || strings.TrimSpace(option.Consequence) == "" || len(option.Consequence) > 2048 || !utf8.ValidString(option.Consequence) || !decisionReturnState(option.ReturnState) {
			return fmt.Errorf("decision_request.options[%d] requires a unique label, consequence and return_state Todo or Rework", i)
		}
		labels[label] = true
		if option.Recommended {
			recommended++
		}
	}
	if recommended != 1 {
		return errors.New("decision_request requires exactly one recommended option")
	}
	return nil
}

func decisionReturnState(state string) bool {
	return state == "Todo" || state == "Rework"
}
