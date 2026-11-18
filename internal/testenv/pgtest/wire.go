package pgtest

import (
	"bytes"
	"strings"
)

type WireCost struct {
	FirstPrepareExchanges int
	ExecutionExchanges    int
	Statements            int
	Transactions          int
}

type FrontendTrace struct {
	seen         map[string]bool
	begins       map[string]bool
	portals      map[string]bool
	firstPrepare bool
	otherWork    bool
}

func (t *FrontendTrace) Count(frame []byte) WireCost {
	var cost WireCost
	body := frame[5:]
	switch frame[0] {
	case 'P':
		name, sql, _ := bytes.Cut(body, []byte{0})
		if t.seen == nil {
			t.seen = make(map[string]bool)
			t.begins = make(map[string]bool)
		}
		key := string(name)
		t.begins[key] = bytes.HasPrefix(sql, []byte("BEGIN "))
		if strings.HasPrefix(key, "stmtcache_") && !t.seen[key] {
			t.seen[key] = true
			t.firstPrepare = true
		} else {
			t.otherWork = true
		}
	case 'D':
		if len(body) == 0 || body[0] != 'S' {
			t.otherWork = true
		}
	case 'B':
		portal, rest, _ := bytes.Cut(body, []byte{0})
		name, _, _ := bytes.Cut(rest, []byte{0})
		if t.portals == nil {
			t.portals = make(map[string]bool)
		}
		t.portals[string(portal)] = t.begins[string(name)]
		t.otherWork = true
	case 'E':
		portal, _, _ := bytes.Cut(body, []byte{0})
		if t.portals[string(portal)] {
			cost.Transactions++
		}
		cost.Statements++
		t.otherWork = true
	case 'Q':
		cost.Statements++
		cost.ExecutionExchanges++
		if bytes.HasPrefix(body, []byte("BEGIN ")) {
			cost.Transactions++
		}
		t.firstPrepare, t.otherWork = false, false
	case 'S':
		if t.firstPrepare && !t.otherWork {
			cost.FirstPrepareExchanges++
		} else {
			cost.ExecutionExchanges++
		}
		t.firstPrepare, t.otherWork = false, false
	default:
		t.otherWork = true
	}
	return cost
}
