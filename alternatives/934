package conversation

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestValidateAnswers(t *testing.T) {
	t.Parallel()
	question := Question{
		ID: "q_1",
		Prompts: []Prompt{
			{ID: "color", Header: "Color", Question: "Pick a color", Options: []Option{{Label: "red"}, {Label: "blue", Description: "calm"}}},
			{ID: "name", Question: "Your name?", FreeText: true},
		},
	}
	tests := []struct {
		name     string
		question Question
		answers  map[string][]string
		wantErr  string
	}{
		{name: "complete", question: question, answers: map[string][]string{"color": {"red"}, "name": {"Ada"}}},
		{name: "multiple values allowed", question: question, answers: map[string][]string{"color": {"red", "blue"}, "name": {"Ada"}}},
		{name: "missing prompt", question: question, answers: map[string][]string{"color": {"red"}}, wantErr: "name"},
		{name: "unknown prompt", question: question, answers: map[string][]string{"color": {"red"}, "name": {"Ada"}, "extra": {"x"}}, wantErr: "extra"},
		{name: "empty answer set", question: question, answers: map[string][]string{"color": {}, "name": {"Ada"}}, wantErr: "color"},
		{name: "nil answers", question: question, answers: nil, wantErr: "color"},
		{name: "no prompts rejects answers", question: Question{ID: "q_2"}, answers: map[string][]string{"a": {"b"}}, wantErr: "a"},
		{name: "no prompts accepts empty", question: Question{ID: "q_2"}, answers: map[string][]string{}},
		{name: "value outside the options", question: question, answers: map[string][]string{"color": {"green"}, "name": {"Ada"}}, wantErr: "green"},
		{name: "one value outside the options", question: question, answers: map[string][]string{"color": {"red", "green"}, "name": {"Ada"}}, wantErr: "green"},
		{name: "free text accepts anything", question: question, answers: map[string][]string{"color": {"blue"}, "name": {"anything at all"}}},
		{name: "prompt without options accepts anything", question: Question{ID: "q_3", Prompts: []Prompt{{ID: "note"}}}, answers: map[string][]string{"note": {"free"}}},
		{name: "free text with options accepts an unlisted value", question: Question{ID: "q_4", Prompts: []Prompt{{ID: "pick", Options: []Option{{Label: "red"}}, FreeText: true}}}, answers: map[string][]string{"pick": {"purple"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAnswers(test.question, test.answers)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateAnswers() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateAnswers() error = nil, want mention of %q", test.wantErr)
			}
			if !errors.Is(err, ErrInvalidAnswers) {
				t.Fatalf("ValidateAnswers() error = %v, want ErrInvalidAnswers", err)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ValidateAnswers() error = %q, want mention of %q", err, test.wantErr)
			}
		})
	}
}

func TestQuestionJSONShape(t *testing.T) {
	t.Parallel()
	expires := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	question := Question{
		ID:             "q_1",
		ConversationID: "conv_1",
		MessageID:      "msg_1",
		Status:         QuestionPending,
		Owner:          Owner{AttemptID: "att_1", TurnID: "turn_1", FencingToken: 3},
		Prompts:        []Prompt{{ID: "p", Header: "H", Question: "Q", Options: []Option{{Label: "L", Description: "D"}}, FreeText: true}},
		Answers:        map[string][]string{"p": {"L"}},
		AnsweredBy:     "principal",
		ExpiresAt:      &expires,
		CreatedAt:      expires,
		UpdatedAt:      expires,
	}
	encoded, err := json.Marshal(question)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, fragment := range []string{
		`"id":"q_1"`, `"conversation_id":"conv_1"`, `"message_id":"msg_1"`, `"status":"pending"`,
		`"owner":{"attempt_id":"att_1","turn_id":"turn_1"}`,
		`"questions":[{"id":"p","header":"H","question":"Q","options":[{"label":"L","description":"D"}],"free_text":true}]`,
		`"answers":{"p":["L"]}`, `"answered_by":"principal"`, `"expires_at":"2026-09-10T00:00:00Z"`,
	} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("question json = %s, missing %s", encoded, fragment)
		}
	}
	if strings.Contains(string(encoded), "fencing_token") {
		t.Fatalf("question json must not expose the full owner tuple: %s", encoded)
	}
	encoded, err = json.Marshal(Question{ID: "q_2", Status: QuestionExpired})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, fragment := range []string{`"answered_by":null`, `"expires_at":null`, `"questions":[]`, `"answers":{}`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("empty question json = %s, missing %s", encoded, fragment)
		}
	}
}

func TestOwnerAndExecutionJSONShape(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	execution := Execution{
		Status:       ExecutionRunning,
		Owner:        Owner{AttemptID: "att_1", RunID: "run_1", LeaseID: "lease_1", FencingToken: 5, RunnerID: "runner_1", MachineID: "machine_1", ThreadID: "thread_1", TurnID: "turn_1"},
		Capabilities: Capabilities{Steer: true, Interrupt: true},
		UpdatedAt:    now,
	}
	encoded, err := json.Marshal(execution)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, fragment := range []string{
		`"status":"running"`, `"attempt_id":"att_1"`, `"run_id":"run_1"`, `"runner_id":"runner_1"`, `"thread_id":"thread_1"`, `"turn_id":"turn_1"`,
		`"capabilities":{"steer":true,"interrupt":true,"answer":false,"continue":false}`, `"error":null`, `"updated_at":"2026-09-09T12:00:00Z"`,
	} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("execution json = %s, missing %s", encoded, fragment)
		}
	}
	var decoded Execution
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded.Owner != execution.Owner || decoded.Status != execution.Status || decoded.Capabilities != execution.Capabilities {
		t.Fatalf("round trip = %#v, want %#v", decoded, execution)
	}
	withError := Execution{Status: ExecutionFailed, Error: "boom"}
	encoded, err = json.Marshal(withError)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(encoded), `"error":"boom"`) {
		t.Fatalf("execution json = %s, want error string", encoded)
	}
}

func TestExecutionUnmarshalRejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	var execution Execution
	if err := json.Unmarshal([]byte(`{"status": 5}`), &execution); err == nil {
		t.Fatal("Unmarshal() error = nil, want type error")
	}
}

// TestValidatePrompts covers the bounds a request for user input must obey.
// A runner is trusted to be correct, not to be bounded, and every prompt is
// rendered in a browser.
func TestValidatePrompts(t *testing.T) {
	t.Parallel()
	long := func(n int) string { return strings.Repeat("x", n) }
	options := func(n int) []Option {
		list := make([]Option, 0, n)
		for i := range n {
			list = append(list, Option{Label: strconv.Itoa(i)})
		}
		return list
	}
	prompts := func(n int) []Prompt {
		list := make([]Prompt, 0, n)
		for i := range n {
			list = append(list, Prompt{ID: strconv.Itoa(i), Question: "?"})
		}
		return list
	}
	tests := []struct {
		name    string
		prompts []Prompt
		wantErr string
	}{
		{name: "one prompt", prompts: []Prompt{{ID: "a", Question: "?"}}},
		{name: "at the prompt limit", prompts: prompts(MaxPrompts)},
		{name: "at the option limit", prompts: []Prompt{{ID: "a", Options: options(MaxPromptOptions)}}},
		{name: "no prompts", wantErr: "at least one"},
		{name: "too many prompts", prompts: prompts(MaxPrompts + 1), wantErr: "at most"},
		{name: "missing id", prompts: []Prompt{{Question: "?"}}, wantErr: "id"},
		{name: "blank id", prompts: []Prompt{{ID: "  ", Question: "?"}}, wantErr: "id"},
		{name: "id too long", prompts: []Prompt{{ID: long(MaxPromptIDBytes + 1)}}, wantErr: "id"},
		{name: "repeated id", prompts: []Prompt{{ID: "a"}, {ID: "a"}}, wantErr: "repeated"},
		{name: "header too long", prompts: []Prompt{{ID: "a", Header: long(MaxPromptHeaderBytes + 1)}}, wantErr: "too long"},
		{name: "question too long", prompts: []Prompt{{ID: "a", Question: long(MaxPromptQuestionBytes + 1)}}, wantErr: "too long"},
		{name: "too many options", prompts: []Prompt{{ID: "a", Options: options(MaxPromptOptions + 1)}}, wantErr: "options"},
		{name: "blank option label", prompts: []Prompt{{ID: "a", Options: []Option{{Label: " "}}}}, wantErr: "option label"},
		{name: "option label too long", prompts: []Prompt{{ID: "a", Options: []Option{{Label: long(MaxOptionLabelBytes + 1)}}}}, wantErr: "option label"},
		{name: "option description too long", prompts: []Prompt{{ID: "a", Options: []Option{{Label: "ok", Description: long(MaxOptionDetailBytes + 1)}}}}, wantErr: "option label"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePrompts(test.prompts)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidatePrompts() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidatePrompts() error = nil, want mention of %q", test.wantErr)
			}
			if !errors.Is(err, ErrInvalidPrompt) {
				t.Fatalf("ValidatePrompts() error = %v, want ErrInvalidPrompt", err)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ValidatePrompts() error = %q, want mention of %q", err, test.wantErr)
			}
		})
	}
}
