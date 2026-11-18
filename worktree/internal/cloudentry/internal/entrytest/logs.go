package entrytest

import (
	"bytes"
	"sync"
)

type LogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *LogBuffer) Take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	value := b.buffer.String()
	b.buffer.Reset()
	return value
}
