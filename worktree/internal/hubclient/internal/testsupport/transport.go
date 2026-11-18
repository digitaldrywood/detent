package testsupport

import (
	"net/http"
	"sync/atomic"
)

type transportValue struct{ transport http.RoundTripper }
type Transport struct {
	current atomic.Pointer[transportValue]
	initial http.RoundTripper
}

func NewTransport(transport http.RoundTripper) *Transport {
	if transport == nil {
		transport = http.DefaultTransport
	}
	result := &Transport{initial: transport}
	result.Store(transport)
	return result
}
func (t *Transport) Load() http.RoundTripper { return t.current.Load().transport }
func (t *Transport) Store(transport http.RoundTripper) {
	t.current.Store(&transportValue{transport: transport})
}
func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t.Load().RoundTrip(request)
}
func (t *Transport) CloseIdleConnections() {
	if closer, ok := t.initial.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	if closer, ok := t.Load().(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
