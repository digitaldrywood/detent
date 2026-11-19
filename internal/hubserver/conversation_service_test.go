package hubserver

import "sync"

type recordingWaker struct {
	mu          sync.Mutex
	woken       []string
	unavailable bool
}

func (w *recordingWaker) Available() bool             { return !w.unavailable }
func (w *recordingWaker) Cancel(string) bool          { return false }
func (w *recordingWaker) Hold(string) (func(), error) { return func() {}, nil }
func (w *recordingWaker) Wake(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.woken = append(w.woken, id)
}
func (w *recordingWaker) Stop() {}
