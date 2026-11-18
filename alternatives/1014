package conversation

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConversationPrompts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{"codex question", `[{"id":"q1","header":"H","question":"Q?","options":[{"label":"A","description":"a"}],"isOther":true}]`, `[{"id":"q1","header":"H","question":"Q?","options":[{"label":"A","description":"a"}],"free_text":true}]`},
		{"missing id and options", `[{"question":"Free?"}]`, `[{"id":"q1","header":"","question":"Free?","options":[],"free_text":true}]`},
		{"empty", `[]`, `[{"id":"q1","header":"","question":"Input requested","options":[],"free_text":true}]`},
		{"invalid", `not json`, `[{"id":"q1","header":"","question":"Input requested","options":[],"free_text":true}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, _ := ProviderPrompts(json.RawMessage(test.raw))
			if string(got) != test.want {
				t.Fatalf("prompts = %s, want %s", got, test.want)
			}
		})
	}
}

func TestConversationPromptsFitTheHubLimits(t *testing.T) {
	t.Parallel()
	options := make([]map[string]string, 0, MaxPromptOptions+3)
	for i := range MaxPromptOptions + 3 {
		options = append(options, map[string]string{"label": fmt.Sprintf("option %d %s", i, strings.Repeat("l", MaxOptionLabelBytes)), "description": strings.Repeat("d", MaxOptionDetailBytes+1)})
	}
	options = append(options, map[string]string{"label": " "})
	oversized := map[string]any{
		"id":       strings.Repeat("i", MaxPromptIDBytes+1),
		"header":   strings.Repeat("é", MaxPromptHeaderBytes),
		"question": strings.Repeat("q", MaxPromptQuestionBytes+1),
		"options":  options,
		"isOther":  false,
	}
	tests := []struct {
		name          string
		questions     []map[string]any
		wantPrompts   int
		wantFreeText  bool
		wantQuestion  string
		wantTruncated bool
	}{
		{name: "oversized fields are cut and marked", questions: []map[string]any{oversized}, wantPrompts: 1, wantFreeText: true, wantQuestion: "3 more options were omitted", wantTruncated: true},
		{name: "extra questions are omitted and named", questions: slices.Repeat([]map[string]any{{"id": "same", "question": "Why?"}}, MaxPrompts+2), wantPrompts: MaxPrompts, wantFreeText: true, wantQuestion: "2 more questions were omitted"},
		{name: "a question within the limits is unchanged", questions: []map[string]any{{"id": "ok", "question": "Fine?", "options": []map[string]string{{"label": "Yes"}}}}, wantPrompts: 1, wantQuestion: "Fine?"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(test.questions)
			if err != nil {
				t.Fatal(err)
			}
			var prompts []Prompt
			encoded, _ := ProviderPrompts(raw)
			if err := json.Unmarshal(encoded, &prompts); err != nil {
				t.Fatal(err)
			}
			if err := ValidatePrompts(prompts); err != nil {
				t.Fatalf("hub validation = %v", err)
			}
			if len(prompts) != test.wantPrompts {
				t.Fatalf("prompts = %d, want %d", len(prompts), test.wantPrompts)
			}
			last := prompts[len(prompts)-1]
			if last.FreeText != test.wantFreeText || !strings.Contains(last.Question, test.wantQuestion) {
				t.Fatalf("last prompt = free text %t, question %q", last.FreeText, last.Question)
			}
			truncated := strings.Contains(last.Question, promptTruncated) && strings.HasSuffix(last.Header, promptTruncated)
			if truncated != test.wantTruncated {
				t.Fatalf("truncation marked = %t, want %t", truncated, test.wantTruncated)
			}
			if test.wantTruncated {
				if !utf8.ValidString(last.Header) || len(last.Options) != MaxPromptOptions || !strings.HasSuffix(last.Options[0].Label, promptTruncated) {
					t.Fatalf("bounded prompt = %#v", last)
				}
			}
		})
	}
}
