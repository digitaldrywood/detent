package templates

import "strings"

func applicationName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Detent"
	}
	return value
}
