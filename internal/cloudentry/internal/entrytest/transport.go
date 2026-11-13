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
	server := httptest.NewRequest(request.Method, request.URL.RequestURI(), request.Body)
	server.Header = request.Header.Clone()
	server = server.WithContext(request.Context())
	h.Handler.ServeHTTP(recorder, server)
	return recorder.Result(), nil
}
