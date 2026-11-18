package pgtest

import "bytes"

type PrepareExchanges struct {
	seen         map[string]bool
	first, other bool
}

func (m *PrepareExchanges) ColdSync(frame []byte) bool {
	switch frame[0] {
	case 'P':
		name, _, _ := bytes.Cut(frame[5:], []byte{0})
		if m.seen == nil {
			m.seen = make(map[string]bool)
		}
		if len(name) == 0 || m.seen[string(name)] {
			m.other = true
		} else {
			m.first = true
			m.seen[string(name)] = true
		}
	case 'E', 'B', 'C':
		m.other = true
	case 'S':
		cold := m.first && !m.other
		m.first, m.other = false, false
		return cold
	}
	return false
}
