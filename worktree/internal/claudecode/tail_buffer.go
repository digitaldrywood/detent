package claudecode

import "sync"

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func newTailBuffer(limit int) *tailBuffer {
	if limit <= 0 {
		limit = defaultStderrTailBytes
	}
	return &tailBuffer{limit: limit}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.limit:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
