package templates

import "fmt"

func operationsNumber(value *float64) string {
	if value == nil {
		return "Unavailable"
	}
	return fmt.Sprintf("%.2f", *value)
}
