package activity

import "strings"

func ValidationPhase(text string) string {
	_, phase := validationProgress(text)
	return phase
}

func ValidationMessage(text string) string {
	message, _ := validationProgress(text)
	return message
}

func validationProgress(text string) (string, string) {
	markers := [...]struct {
		text  string
		phase string
	}{
		{"validation gate waiting:", "waiting_validation"},
		{"validation gate acquired shared lock;", "validating"},
		{"validation gate running:", "validating"},
		{"validation gate finished:", ""},
		{"validation queue wait ended", ""},
		{"validation canceled before command start:", ""},
		{"acquire validation lock:", ""},
	}
	message, phase := "", ""
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		for _, marker := range markers {
			if strings.HasPrefix(line, marker.text) {
				message, phase = line, marker.phase
				break
			}
		}
	}
	return message, phase
}
