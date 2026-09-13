package serviceapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"strings"
)

const (
	AddressEnvironment          = "DETENT_SERVICE_ADDRESS"
	TokenEnvironment            = "DETENT_API_TOKEN"
	DispositionTokenEnvironment = "DETENT_AUDIT_DISPOSITION_TOKEN"

	// WorkerDispositionScope is the only service operation granted by worker
	// credentials. It is intentionally not an API-key scope.
	WorkerDispositionScope = "worker:security-audit-disposition"
	workerTokenPrefix      = "detent_worker_v1_"
)

// WorkerCredential identifies the project authorized by a worker token.
type WorkerCredential struct {
	ProjectID string
}

// WorkerCredentials issues process-lifetime, project-scoped worker tokens.
// The operation is fixed to WorkerDispositionScope by the token signature.
type WorkerCredentials struct {
	secret [sha256.Size]byte
}

// NewWorkerCredentials creates an isolated worker credential issuer for the
// running Detent process. Tokens from a previous process become invalid.
func NewWorkerCredentials() (*WorkerCredentials, error) {
	return newWorkerCredentials(rand.Reader)
}

func newWorkerCredentials(reader io.Reader) (*WorkerCredentials, error) {
	credentials := &WorkerCredentials{}
	if _, err := io.ReadFull(reader, credentials.secret[:]); err != nil {
		return nil, err
	}
	return credentials, nil
}

// Token returns a credential limited to recording security-audit dispositions
// for projectID.
func (c *WorkerCredentials) Token(projectID string) string {
	projectID = strings.TrimSpace(projectID)
	if c == nil || projectID == "" {
		return ""
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(projectID))
	signature := base64.RawURLEncoding.EncodeToString(c.signature(projectID))
	return workerTokenPrefix + payload + "." + signature
}

// Authenticate validates a worker token and returns its project boundary.
func (c *WorkerCredentials) Authenticate(token string) (WorkerCredential, bool) {
	if c == nil || len(token) > 4096 {
		return WorkerCredential{}, false
	}
	remainder, ok := strings.CutPrefix(strings.TrimSpace(token), workerTokenPrefix)
	if !ok {
		return WorkerCredential{}, false
	}
	payload, encodedSignature, ok := strings.Cut(remainder, ".")
	if !ok || payload == "" || encodedSignature == "" || strings.Contains(encodedSignature, ".") {
		return WorkerCredential{}, false
	}
	projectBytes, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return WorkerCredential{}, false
	}
	projectID := strings.TrimSpace(string(projectBytes))
	if projectID == "" || projectID != string(projectBytes) {
		return WorkerCredential{}, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil || !hmac.Equal(signature, c.signature(projectID)) {
		return WorkerCredential{}, false
	}
	return WorkerCredential{ProjectID: projectID}, true
}

func (c *WorkerCredentials) signature(projectID string) []byte {
	mac := hmac.New(sha256.New, c.secret[:])
	_, _ = mac.Write([]byte(WorkerDispositionScope))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(projectID))
	return mac.Sum(nil)
}

// Connection identifies the running Detent service available to a worker.
type Connection struct {
	Address          string
	DispositionToken string
}

// Environment returns explicit worker process overrides for the connection.
func (c Connection) Environment() map[string]string {
	return map[string]string{
		AddressEnvironment:          c.Address,
		TokenEnvironment:            "",
		DispositionTokenEnvironment: c.DispositionToken,
	}
}

// RestrictedEnvironment clears inherited service credentials from worker
// roles that do not call the running Detent service.
func RestrictedEnvironment() map[string]string {
	return map[string]string{
		AddressEnvironment:          "",
		TokenEnvironment:            "",
		DispositionTokenEnvironment: "",
	}
}
