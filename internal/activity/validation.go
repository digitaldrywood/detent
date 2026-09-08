package activity

import "strings"

func ValidationPhase(text string) string {
	latest, phase := -1, ""
	for _, marker := range []struct {
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
	} {
		if position := strings.LastIndex(text, marker.text); position > latest {
			latest, phase = position, marker.phase
		}
	}
	return phase
}
