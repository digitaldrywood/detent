package templates

import (
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/operations"
)

func operationsQuestionAge(question operations.Decision, now time.Time) string {
	if question.AskedAt == nil {
		return "age unknown"
	}
	return cardFactAge(now, *question.AskedAt)
}

func operationsQuestionSummary(report operations.Report) string {
	count := 0
	var oldest *time.Time
	unknown := 0
	for _, question := range report.Decisions {
		if question.Kind != "question" {
			continue
		}
		count++
		if question.AskedAt == nil {
			unknown++
			continue
		}
		if oldest == nil || question.AskedAt.Before(*oldest) {
			oldest = question.AskedAt
		}
	}
	summary := fmt.Sprintf("Open questions: %d", count)
	if oldest != nil {
		summary += " · Oldest: " + cardFactAge(report.DataTime, *oldest)
	}
	if unknown > 0 {
		summary += fmt.Sprintf(" · Unknown age: %d", unknown)
	}
	return summary
}
