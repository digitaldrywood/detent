package testsupport

import (
	"net/http"
	"net/http/httptest"
)

type RoundTrip func(*http.Request) (*http.Response, error)

func (f RoundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
func HandlerClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: RoundTrip(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		incoming := request.Clone(request.Context())
		incoming.RequestURI = incoming.URL.RequestURI()
		handler.ServeHTTP(recorder, incoming)
		return recorder.Result(), nil
	})}
}
