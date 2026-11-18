package conversation

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// promptTruncated marks text the runner cut to fit the hub's limits.
const promptTruncated = "…"

// ProviderPrompts converts the provider's raw questions into the hub's
// prompt shape. Missing identifiers are generated and an unreadable or empty
// request becomes one free-text prompt, so a question can always be shown.
// Every field is bounded to the limits the hub validates, so an oversized
// question is shown truncated rather than refused: cut text ends with an
// ellipsis, and omitted options or questions are named in the question text.
// It also returns each id it generated or trimmed in place of the provider's
// id, including a missing or blank one, mapped to that provider id.
func ProviderPrompts(raw json.RawMessage) (json.RawMessage, map[string]string) {
	var questions []struct {
		ID       string `json:"id"`
		Header   string `json:"header"`
		Question string `json:"question"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
		IsOther  *bool `json:"isOther"`
		FreeText *bool `json:"free_text"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &questions); err != nil {
			questions = nil
		}
	}
	omittedQuestions := max(len(questions)-MaxPrompts, 0)
	questions = questions[:min(len(questions), MaxPrompts)]
	prompts := make([]Prompt, 0, max(len(questions), 1))
	seen := make(map[string]struct{}, len(questions))
	generated := make(map[string]string)
	for i, question := range questions {
		prompt := Prompt{ID: strings.TrimSpace(question.ID), Header: question.Header, Options: []Option{}}
		if prompt.ID == "" || len(prompt.ID) > MaxPromptIDBytes {
			prompt.ID = "q" + strconv.Itoa(i+1)
		}
		for suffix := 2; ; suffix++ {
			if _, duplicate := seen[prompt.ID]; !duplicate {
				break
			}
			prompt.ID = "q" + strconv.Itoa(i+1) + "-" + strconv.Itoa(suffix)
		}
		seen[prompt.ID] = struct{}{}
		if question.ID != prompt.ID {
			generated[prompt.ID] = question.ID
		}
		omittedOptions := 0
		for _, option := range question.Options {
			label := strings.TrimSpace(option.Label)
			if label == "" {
				continue
			}
			if len(prompt.Options) == MaxPromptOptions {
				omittedOptions++
				continue
			}
			prompt.Options = append(prompt.Options, Option{
				Label:       boundPromptBytes(label, MaxOptionLabelBytes),
				Description: boundPromptBytes(option.Description, MaxOptionDetailBytes),
			})
		}
		switch {
		case question.FreeText != nil:
			prompt.FreeText = *question.FreeText
		case question.IsOther != nil:
			prompt.FreeText = *question.IsOther
		default:
			prompt.FreeText = len(prompt.Options) == 0
		}
		// The notes survive the bound: the question text is cut to make room
		// for them.
		var notes string
		if omittedOptions > 0 {
			prompt.FreeText = true
			notes += fmt.Sprintf("\n\n(%d more options were omitted; answer in free text if none fits.)", omittedOptions)
		}
		if omittedQuestions > 0 && i == len(questions)-1 {
			notes += fmt.Sprintf("\n\n(%d more questions were omitted.)", omittedQuestions)
		}
		prompt.Header = boundPromptBytes(prompt.Header, MaxPromptHeaderBytes)
		prompt.Question = boundPromptBytes(question.Question, MaxPromptQuestionBytes-len(notes)) + notes
		prompts = append(prompts, prompt)
	}
	if len(prompts) == 0 {
		prompts = append(prompts, Prompt{ID: "q1", Question: "Input requested", Options: []Option{}, FreeText: true})
	}
	encoded, err := json.Marshal(prompts)
	if err != nil {
		return json.RawMessage(`[{"id":"q1","header":"","question":"Input requested","options":[],"free_text":true}]`), nil
	}
	return encoded, generated
}

// boundPromptBytes cuts value to at most limit bytes on a rune
// boundary, ending cut text with promptTruncated.
func boundPromptBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return TruncateBytes(value, limit-len(promptTruncated)) + promptTruncated
}
