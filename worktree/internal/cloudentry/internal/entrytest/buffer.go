package entrytest

import (
	"bytes"
	"sync"
)

type LockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *LockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *LockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
