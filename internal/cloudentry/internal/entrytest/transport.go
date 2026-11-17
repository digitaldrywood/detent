package entrytest

import (
	"net/http"
	"net/http/httptest"
)

type HandlerTransport struct {
	Handler http.Handler
}

func (h HandlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	server := httptest.NewRequestWithContext(request.Context(), request.Method, request.URL.RequestURI(), request.Body)
	server.Header = request.Header.Clone()
	h.Handler.ServeHTTP(recorder, server)
	return recorder.Result(), nil
}
