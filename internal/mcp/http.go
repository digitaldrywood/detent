package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

const (
	MaxHTTPRequestBytes  = maxFrameBytes
	MaxHTTPResponseBytes = 3*operatortool.MaxResultBytes + 64*1024

	defaultMaxHTTPSessions = 1024
	defaultHTTPSessionIdle = 30 * time.Minute
	httpSessionHeader      = "Mcp-Session-Id"
	httpProtocolHeader     = "Mcp-Protocol-Version"
	codeInternalError      = -32603
)

type HTTPConfig struct {
	Logger             *slog.Logger
	Principal          func(*http.Request) operatortool.Identity
	MaxSessions        int
	SessionIdleTimeout time.Duration
	Now                func() time.Time
	GenerateSessionID  func() (string, error)
}

type HTTPHandler struct {
	logger             *slog.Logger
	executor           Executor
	version            string
	principal          func(*http.Request) operatortool.Identity
	maxSessions        int
	sessionIdleTimeout time.Duration
	now                func() time.Time
	generateSessionID  func() (string, error)

	mu       sync.Mutex
	sessions map[string]*httpProtocolSession
	stopped  bool
}

func NewHTTPHandler(executor Executor, version string, cfg HTTPConfig) *HTTPHandler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxSessions := cfg.MaxSessions
	if maxSessions <= 0 {
		maxSessions = defaultMaxHTTPSessions
	}
	sessionIdleTimeout := cfg.SessionIdleTimeout
	if sessionIdleTimeout <= 0 {
		sessionIdleTimeout = defaultHTTPSessionIdle
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	generateSessionID := cfg.GenerateSessionID
	if generateSessionID == nil {
		generateSessionID = newHTTPSessionID
	}
	return &HTTPHandler{
		logger:             logger,
		executor:           executor,
		version:            version,
		principal:          cfg.Principal,
		maxSessions:        maxSessions,
		sessionIdleTimeout: sessionIdleTimeout,
		now:                now,
		generateSessionID:  generateSessionID,
		sessions:           make(map[string]*httpProtocolSession),
	}
}

func (h *HTTPHandler) ServeHTTP(writer http.ResponseWriter, req *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if !sameOriginRequest(req) {
		writeHTTPTransportError(writer, http.StatusForbidden, "request origin is not allowed")
		return
	}
	switch req.Method {
	case http.MethodPost:
		h.servePost(writer, req)
	case http.MethodDelete:
		h.serveDelete(writer, req)
	case http.MethodGet:
		writer.Header().Set("Allow", "POST, DELETE")
		writeHTTPTransportError(writer, http.StatusMethodNotAllowed, "server-sent events are not supported")
	default:
		writer.Header().Set("Allow", "POST, DELETE")
		writeHTTPTransportError(writer, http.StatusMethodNotAllowed, "method is not allowed")
	}
}

func (h *HTTPHandler) servePost(writer http.ResponseWriter, req *http.Request) {
	if !isJSONContentType(req.Header.Get("Content-Type")) {
		writeHTTPTransportError(writer, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	body, err := readHTTPRequest(writer, req)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeHTTPTransportError(writer, http.StatusRequestEntityTooLarge, "MCP request exceeds the size limit")
			return
		}
		writeHTTPTransportError(writer, http.StatusBadRequest, "MCP request could not be read")
		return
	}
	message, direct := parseHTTPRequest(body)
	if direct != nil {
		writeHTTPResponse(writer, http.StatusOK, direct)
		return
	}

	meta, problem := metadata(message)
	if meta.Modern || req.Header.Get(httpProtocolHeader) == ProtocolVersion || message.Method == "server/discover" {
		if problem == nil && !meta.Modern {
			problem = &rpcError{Code: codeInvalidParams, Message: "Protocol metadata is required"}
		}
		if problem != nil {
			writeHTTPResponse(writer, http.StatusBadRequest, marshalHTTPResponse(response{JSONRPC: "2.0", ID: responseID(message.ID), Error: problem}))
			return
		}
		if problem = validateModernHeaders(req, message, meta); problem != nil {
			writeHTTPResponse(writer, http.StatusBadRequest, marshalHTTPResponse(response{JSONRPC: "2.0", ID: responseID(message.ID), Error: problem}))
			return
		}
		h.serveModern(writer, req, message, body)
		return
	}
	if version := req.Header.Get(httpProtocolHeader); version != "" && negotiateVersion(version) != version {
		writeHTTPResponse(writer, http.StatusBadRequest, marshalHTTPResponse(response{JSONRPC: "2.0", ID: responseID(message.ID), Error: &rpcError{Code: codeUnsupportedVersion, Message: "Unsupported protocol version", Data: map[string]any{"supported": supportedVersions()}}}))
		return
	}
	principal := h.requestPrincipal(req)
	sessionID := strings.TrimSpace(req.Header.Get(httpSessionHeader))
	if sessionID == "" {
		if message.Method != "initialize" || len(message.ID) == 0 {
			writeHTTPTransportError(writer, http.StatusBadRequest, "valid MCP session ID is required")
			return
		}
		h.initializeSession(writer, req, principal, message, body)
		return
	}

	session := h.session(sessionID, principal)
	if session == nil {
		writeHTTPTransportError(writer, http.StatusNotFound, "MCP session not found")
		return
	}
	if version := strings.TrimSpace(req.Header.Get(httpProtocolHeader)); version != "" && version != session.protocolVersion() {
		writeHTTPTransportError(writer, http.StatusBadRequest, "MCP protocol version does not match the session")
		return
	}
	h.dispatch(writer, req, session, message, body)
}

func (h *HTTPHandler) initializeSession(writer http.ResponseWriter, req *http.Request, principal operatortool.Identity, message request, body []byte) {
	sessionID, err := h.generateSessionID()
	if err != nil {
		writeHTTPTransportError(writer, http.StatusServiceUnavailable, "MCP session could not be created")
		return
	}
	session := newHTTPProtocolSession(sessionID, principal, h.executor, h.version, h.now(), h.logger)
	session.now, session.idle = h.now, h.sessionIdleTimeout
	frame, notification, err := session.dispatch(req.Context(), message, body)
	if err != nil {
		session.stop()
		session.wait()
		writeHTTPTransportError(writer, http.StatusServiceUnavailable, "MCP session could not be initialized")
		return
	}
	if notification || responseHasError(frame) {
		session.stop()
		session.wait()
		writeHTTPResponse(writer, http.StatusOK, frame)
		return
	}
	if !h.addSession(session) {
		session.stop()
		session.wait()
		writeHTTPTransportError(writer, http.StatusServiceUnavailable, "MCP session capacity is unavailable")
		return
	}
	writer.Header().Set(httpSessionHeader, sessionID)
	writeHTTPResponse(writer, http.StatusOK, frame)
}

func (h *HTTPHandler) dispatch(writer http.ResponseWriter, req *http.Request, session *httpProtocolSession, message request, body []byte) {
	frame, notification, err := session.dispatch(req.Context(), message, body)
	if err != nil {
		if errors.Is(err, errHTTPSessionClosed) {
			h.removeSession(session.id, session)
			writeHTTPTransportError(writer, http.StatusNotFound, "MCP session not found")
			return
		}
		writeHTTPTransportError(writer, http.StatusServiceUnavailable, "MCP request could not be completed")
		return
	}
	if notification {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	writeHTTPResponse(writer, http.StatusOK, frame)
}

func (h *HTTPHandler) serveDelete(writer http.ResponseWriter, req *http.Request) {
	sessionID := strings.TrimSpace(req.Header.Get(httpSessionHeader))
	if sessionID == "" {
		writeHTTPTransportError(writer, http.StatusBadRequest, "valid MCP session ID is required")
		return
	}
	session := h.takeSession(sessionID, h.requestPrincipal(req))
	if session == nil {
		writeHTTPTransportError(writer, http.StatusNotFound, "MCP session not found")
		return
	}
	session.stop()
	done := make(chan struct{})
	go func() {
		session.wait()
		close(done)
	}()
	select {
	case <-done:
		writer.WriteHeader(http.StatusNoContent)
	case <-req.Context().Done():
		writeHTTPTransportError(writer, http.StatusServiceUnavailable, "MCP session shutdown did not complete")
	}
}

func (h *HTTPHandler) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return nil
	}
	h.stopped = true
	sessions := make([]*httpProtocolSession, 0, len(h.sessions))
	for _, session := range h.sessions {
		sessions = append(sessions, session)
	}
	clear(h.sessions)
	h.mu.Unlock()

	for _, session := range sessions {
		session.stop()
	}
	done := make(chan struct{})
	go func() {
		for _, session := range sessions {
			session.wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *HTTPHandler) addSession(session *httpProtocolSession) bool {
	now := h.now()
	var expired []*httpProtocolSession
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return false
	}
	for id, candidate := range h.sessions {
		if now.Sub(candidate.lastSeen()) >= h.sessionIdleTimeout {
			delete(h.sessions, id)
			expired = append(expired, candidate)
		}
	}
	if len(h.sessions) >= h.maxSessions || h.sessions[session.id] != nil {
		h.mu.Unlock()
		stopHTTPSessions(expired)
		return false
	}
	h.sessions[session.id] = session
	h.mu.Unlock()
	stopHTTPSessions(expired)
	return true
}

func (h *HTTPHandler) session(id string, principal operatortool.Identity) *httpProtocolSession {
	now := h.now()
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return nil
	}
	session := h.sessions[id]
	if session == nil || session.principal != principal {
		h.mu.Unlock()
		return nil
	}
	if now.Sub(session.lastSeen()) >= h.sessionIdleTimeout {
		delete(h.sessions, id)
		h.mu.Unlock()
		session.stop()
		go session.wait()
		return nil
	}
	session.touch(now)
	h.mu.Unlock()
	return session
}

func (h *HTTPHandler) takeSession(id string, principal operatortool.Identity) *httpProtocolSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	session := h.sessions[id]
	if session == nil || session.principal != principal {
		return nil
	}
	delete(h.sessions, id)
	return session
}

func (h *HTTPHandler) removeSession(id string, session *httpProtocolSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions[id] == session {
		delete(h.sessions, id)
	}
}

func (h *HTTPHandler) requestPrincipal(req *http.Request) operatortool.Identity {
	if h.principal == nil {
		return operatortool.Identity{}
	}
	return h.principal(req)
}

func stopHTTPSessions(sessions []*httpProtocolSession) {
	for _, session := range sessions {
		session.stop()
		go session.wait()
	}
}

var errHTTPSessionClosed = errors.New("MCP HTTP session is closed")

type httpProtocolSession struct {
	id           string
	connectionID string
	modern       bool
	principal    operatortool.Identity
	lifetime     context.Context //nolint:containedctx // The protocol session owns cancellation across POSTs and handler shutdown.
	done         <-chan struct{}
	cancel       context.CancelFunc
	protocol     *session
	router       *httpResponseRouter

	mu       sync.Mutex
	closing  bool
	inflight sync.WaitGroup
	seenAt   atomic.Int64
	now      func() time.Time
	idle     time.Duration
}

func newHTTPProtocolSession(id string, principal operatortool.Identity, executor Executor, version string, now time.Time, logger *slog.Logger) *httpProtocolSession {
	ctx, cancel := context.WithCancel(context.Background())
	router := newHTTPResponseRouter()
	protocol := &session{
		done:     ctx.Done(),
		cancel:   cancel,
		executor: executor,
		logger:   logger,
		version:  strings.TrimSpace(version),
		output:   router,
		state:    stateNew,
		active:   make(map[string]*activeRequest),
	}
	if protocol.version == "" {
		protocol.version = "dev"
	}
	result := &httpProtocolSession{id: id, principal: principal, lifetime: ctx, done: ctx.Done(), cancel: cancel, protocol: protocol, router: router}
	result.touch(now)
	return result
}

func (s *httpProtocolSession) dispatch(ctx context.Context, message request, frame []byte) ([]byte, bool, error) {
	if !s.begin() {
		return nil, false, errHTTPSessionClosed
	}
	defer s.inflight.Done()
	if len(message.ID) == 0 {
		if err := s.protocol.handle(s.sessionContext(ctx), frame); err != nil {
			return nil, true, err
		}
		return nil, true, nil
	}
	key, ok := requestIDKey(message.ID)
	if !ok {
		return marshalHTTPResponse(response{JSONRPC: "2.0", ID: json.RawMessage(`null`), Error: &rpcError{Code: codeInvalidRequest, Message: "Invalid Request"}}), false, nil
	}
	responses, ok := s.router.register(key)
	if !ok {
		return marshalHTTPResponse(response{JSONRPC: "2.0", ID: responseID(message.ID), Error: &rpcError{Code: codeInvalidRequest, Message: "Duplicate request ID"}}), false, nil
	}
	defer s.router.unregister(key, responses)
	if err := s.protocol.handle(s.sessionContext(ctx), frame); err != nil {
		return nil, false, err
	}
	select {
	case result := <-responses:
		return result, false, nil
	case <-ctx.Done():
		if err := s.cancelRequest(s.sessionContext(ctx), message.ID); err != nil {
			return nil, false, errors.Join(ctx.Err(), err)
		}
		return nil, false, ctx.Err()
	case <-s.done:
		return nil, false, errHTTPSessionClosed
	}
}

func (s *httpProtocolSession) cancelRequest(ctx context.Context, id json.RawMessage) error {
	params, err := json.Marshal(struct {
		RequestID json.RawMessage `json:"requestId"`
	}{RequestID: id})
	if err != nil {
		return err
	}
	frame, err := json.Marshal(request{JSONRPC: "2.0", Method: "notifications/cancelled", Params: params})
	if err != nil {
		return err
	}
	return s.protocol.handle(ctx, frame)
}

func (s *httpProtocolSession) sessionContext(request context.Context) context.Context {
	connection := operatortool.CurrentConnection(request)
	resolve := connection.Resolve
	connection.ID = s.id
	if s.modern {
		connection.ID = s.connectionID
		return operatortool.WithConnection(request, connection)
	}
	connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing || resolve == nil || s.now != nil && s.idle > 0 && s.now().Sub(s.lastSeen()) >= s.idle {
			return operatortool.Authority{}, operatortool.ErrAccessDenied
		}
		return resolve(ctx)
	}
	request = operatortool.WithConnection(request, connection)
	return httpSessionContext{Context: context.WithoutCancel(request), done: s.done}
}

func (s *httpProtocolSession) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.inflight.Add(1)
	return true
}

func (s *httpProtocolSession) stop() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	s.cancel()
	s.protocol.cancelActive()
	s.mu.Unlock()
}

func (s *httpProtocolSession) wait() {
	s.inflight.Wait()
	s.protocol.calls.Wait()
	s.router.stop()
}

func (s *httpProtocolSession) protocolVersion() string {
	s.protocol.mu.Lock()
	defer s.protocol.mu.Unlock()
	return s.protocol.protocolVersion
}

func (s *httpProtocolSession) touch(now time.Time) {
	s.seenAt.Store(now.UnixNano())
}

func (s *httpProtocolSession) lastSeen() time.Time {
	return time.Unix(0, s.seenAt.Load())
}

type httpResponseRouter struct {
	mu      sync.Mutex
	pending map[string]chan []byte
	stopped bool
}

func newHTTPResponseRouter() *httpResponseRouter {
	return &httpResponseRouter{pending: make(map[string]chan []byte)}
}

func (r *httpResponseRouter) register(key string) (chan []byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.pending[key] != nil {
		return nil, false
	}
	responses := make(chan []byte, 1)
	r.pending[key] = responses
	return responses, true
}

func (r *httpResponseRouter) unregister(key string, responses chan []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[key] == responses {
		delete(r.pending, key)
	}
}

func (r *httpResponseRouter) Write(frame []byte) (int, error) {
	trimmed := bytes.TrimSpace(frame)
	var message struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(trimmed, &message); err != nil {
		return 0, err
	}
	key, ok := requestIDKey(message.ID)
	if !ok {
		return len(frame), nil
	}
	if len(trimmed) > MaxHTTPResponseBytes {
		trimmed = marshalHTTPResponse(response{
			JSONRPC: "2.0",
			ID:      responseID(message.ID),
			Error:   &rpcError{Code: codeInternalError, Message: "MCP response exceeds the size limit"},
		})
	}
	r.mu.Lock()
	responses := r.pending[key]
	stopped := r.stopped
	r.mu.Unlock()
	if stopped || responses == nil {
		return len(frame), nil
	}
	responses <- append([]byte(nil), trimmed...)
	return len(frame), nil
}

func (r *httpResponseRouter) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	clear(r.pending)
}

func parseHTTPRequest(body []byte) (request, []byte) {
	trimmed := bytes.TrimSpace(body)
	if !json.Valid(trimmed) {
		return request{}, marshalHTTPResponse(response{JSONRPC: "2.0", ID: json.RawMessage(`null`), Error: &rpcError{Code: codeParseError, Message: "Parse error"}})
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return request{}, marshalHTTPResponse(response{JSONRPC: "2.0", ID: json.RawMessage(`null`), Error: &rpcError{Code: codeInvalidRequest, Message: "Invalid Request"}})
	}
	var message request
	if err := json.Unmarshal(trimmed, &message); err != nil {
		return request{}, marshalHTTPResponse(response{JSONRPC: "2.0", ID: json.RawMessage(`null`), Error: &rpcError{Code: codeInvalidRequest, Message: "Invalid Request"}})
	}
	if message.JSONRPC != "2.0" || strings.TrimSpace(message.Method) == "" {
		if len(message.ID) == 0 {
			return message, nil
		}
		return request{}, marshalHTTPResponse(response{JSONRPC: "2.0", ID: json.RawMessage(`null`), Error: &rpcError{Code: codeInvalidRequest, Message: "Invalid Request"}})
	}
	if len(message.ID) > 0 {
		if _, ok := requestIDKey(message.ID); !ok {
			return request{}, marshalHTTPResponse(response{JSONRPC: "2.0", ID: json.RawMessage(`null`), Error: &rpcError{Code: codeInvalidRequest, Message: "Invalid Request"}})
		}
	}
	return message, nil
}

func readHTTPRequest(writer http.ResponseWriter, req *http.Request) ([]byte, error) {
	body := http.MaxBytesReader(writer, req.Body, MaxHTTPRequestBytes)
	defer body.Close()
	return io.ReadAll(body)
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	return err == nil && mediaType == "application/json"
}

func sameOriginRequest(req *http.Request) bool {
	origin := strings.TrimSpace(req.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	scheme := "http"
	host := req.Host
	if req.TLS != nil {
		scheme = "https"
	}
	// The application may supply its configured public origin behind TLS
	// termination. Never infer trust from caller-supplied forwarding headers.
	if public, err := url.Parse(operatortool.CurrentConnection(req.Context()).DashboardURL); err == nil && public != nil && public.Host != "" && (public.Scheme == "https" || public.Scheme == "http") {
		scheme, host = public.Scheme, public.Host
	}
	return err == nil && parsed != nil && parsed.User == nil && parsed.Opaque == "" && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Scheme == scheme && parsed.Host != "" && strings.EqualFold(parsed.Host, host)
}

func responseHasError(frame []byte) bool {
	var message struct {
		Error *rpcError `json:"error"`
	}
	return json.Unmarshal(frame, &message) != nil || message.Error != nil
}

func writeHTTPTransportError(writer http.ResponseWriter, status int, message string) {
	writeHTTPResponse(writer, status, marshalHTTPResponse(response{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`null`),
		Error:   &rpcError{Code: -32000, Message: message},
	}))
}

func writeHTTPResponse(writer http.ResponseWriter, status int, frame []byte) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(frame) // #nosec G705 -- application/json response containing encoding/json output, not HTML
}

func marshalHTTPResponse(message response) []byte {
	frame, err := json.Marshal(message)
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Internal error"}}`)
	}
	return frame
}

func newHTTPSessionID() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

type httpSessionContext struct {
	context.Context //nolint:containedctx // Implement Context by preserving request values with the protocol session's cancellation lifetime.
	done            <-chan struct{}
}

func (c httpSessionContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (c httpSessionContext) Done() <-chan struct{} {
	return c.done
}

func (c httpSessionContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

// A modern POST has request-scoped cancellation/routing only. Its application
// connection belongs to the authenticated credential, never an MCP session ID
// or client-supplied metadata. Existing chat approval/mode ownership applies.
func (h *HTTPHandler) serveModern(writer http.ResponseWriter, req *http.Request, message request, body []byte) {
	principal := h.requestPrincipal(req)
	if !principal.Valid() {
		writeHTTPTransportError(writer, http.StatusForbidden, operatortool.ErrAccessDenied.Error())
		return
	}
	id, err := newHTTPSessionID()
	if err != nil {
		writeHTTPTransportError(writer, http.StatusServiceUnavailable, "MCP request could not be started")
		return
	}
	current := operatortool.CurrentConnection(req.Context())
	applicationID := current.ID
	if applicationID == "" {
		raw, err := json.Marshal(principal)
		if err != nil {
			writeHTTPTransportError(writer, http.StatusInternalServerError, "MCP connection could not be identified")
			return
		}
		digest := sha256.Sum256(raw)
		applicationID = "mcp-" + base64.RawURLEncoding.EncodeToString(digest[:])
	}
	requestSession := newHTTPProtocolSession(id, principal, h.executor, h.version, h.now(), h.logger)
	requestSession.modern = true
	requestSession.connectionID = applicationID
	if !h.addSession(requestSession) {
		requestSession.stop()
		requestSession.wait()
		writeHTTPTransportError(writer, http.StatusServiceUnavailable, "MCP request capacity is unavailable")
		return
	}
	defer func() { h.removeSession(id, requestSession); requestSession.stop(); requestSession.wait() }()
	dispatchContext, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(requestSession.lifetime, cancel)
	defer func() { stop(); cancel() }()
	frame, notification, err := requestSession.dispatch(dispatchContext, message, body)
	if err != nil {
		writeHTTPTransportError(writer, http.StatusServiceUnavailable, "MCP request could not be completed")
		return
	}
	if notification {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	status := http.StatusOK
	var result response
	if json.Unmarshal(frame, &result) == nil && result.Error != nil {
		switch result.Error.Code {
		case codeMethodNotFound:
			status = http.StatusNotFound
		case codeInvalidParams, codeUnsupportedVersion, codeHeaderMismatch:
			status = http.StatusBadRequest
		}
	}
	writeHTTPResponse(writer, status, frame)
}

func validateModernHeaders(req *http.Request, message request, meta requestMetadata) *rpcError {
	// Notifications have no required request headers in the modern core.
	if len(message.ID) == 0 {
		return nil
	}
	mismatch := func() *rpcError {
		return &rpcError{Code: codeHeaderMismatch, Message: "MCP request headers are missing or do not match the body"}
	}
	if req.Header.Get(httpProtocolHeader) != meta.Version || req.Header.Get("Mcp-Method") != message.Method {
		return mismatch()
	}
	if message.Method == "tools/call" || message.Method == "resources/read" || message.Method == "prompts/get" {
		var params struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
		}
		if decodeObject(message.Params, &params, false) != nil {
			return mismatch()
		}
		name := params.Name
		if message.Method == "resources/read" {
			name = params.URI
		}
		header, ok := decodeNameHeader(req.Header.Get("Mcp-Name"))
		if !ok || header == "" || header != name {
			return mismatch()
		}
	}
	return nil
}

func decodeNameHeader(value string) (string, bool) {
	if len(value) > 1024 {
		return "", false
	}
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(value, "=?base64?"), "?="))
		return string(decoded), err == nil
	}
	if strings.TrimSpace(value) != value {
		return "", false
	}
	for _, c := range value {
		if c < 0x20 || c > 0x7e {
			return "", false
		}
	}
	return value, true
}
