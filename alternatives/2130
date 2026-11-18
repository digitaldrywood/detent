package hubapi

import "time"

const SearchTermLimit = 32

func MinTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func MaxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func StringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func BoundRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func ConversationTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

func LesserHostedRole(a, b string) string {
	rank := map[string]int{"viewer": 0, "reporter": 1, "member": 2, "admin": 3, "owner": 4}
	if rank[a] <= rank[b] {
		return a
	}
	return b
}
