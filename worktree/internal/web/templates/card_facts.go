package templates

import (
	"fmt"
	"time"
)

func cardFactAge(now, at time.Time) string {
	age := max(time.Duration(0), now.Sub(at))
	if age < time.Minute {
		return "<1m"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age.Minutes()))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age.Hours()))
	}
	return fmt.Sprintf("%dd", int(age.Hours()/24))
}
