package displayorder

import (
	"cmp"
	"time"
)

const (
	PriorityUrgent = iota
	PriorityHigh
	PriorityNormal
	PriorityLow
)

type Item struct {
	Priority       *int
	LastActivityAt time.Time
	Identifier     string
}

func Compare(terminal bool, left, right Item) int {
	if !terminal {
		if order := cmp.Compare(priorityRank(left.Priority), priorityRank(right.Priority)); order != 0 {
			return order
		}
	}
	if left.LastActivityAt.IsZero() != right.LastActivityAt.IsZero() {
		if left.LastActivityAt.IsZero() {
			return 1
		}
		return -1
	}
	if order := right.LastActivityAt.Compare(left.LastActivityAt); order != 0 {
		return order
	}
	return cmp.Compare(left.Identifier, right.Identifier)
}

func priorityRank(priority *int) int {
	if priority == nil || *priority < PriorityUrgent || *priority > PriorityLow {
		return PriorityLow + 1
	}
	return *priority
}
