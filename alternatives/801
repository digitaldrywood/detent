package config

import (
	"errors"
	"strings"
)

func ResolveProgressReview(prompt string) (string, error) {
	var section *markdownHeading
	end := len(prompt)
	for _, heading := range markdownHeadings(prompt) {
		if heading.Level == 2 && strings.EqualFold(strings.TrimSpace(heading.Title), "Progress Review") {
			if section != nil {
				return "", errors.New("workflow Progress Review section is duplicated")
			}
			copy := heading
			section = &copy
		} else if section != nil && end == len(prompt) && heading.Level <= 2 {
			end = heading.Start
		}
	}
	if section == nil {
		return "", nil
	}
	return strings.TrimSpace(prompt[section.End:end]), nil
}
