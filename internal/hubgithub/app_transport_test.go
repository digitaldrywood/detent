package hubgithub

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
)

func TestAppTransportRepositoryRouting(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var mu sync.Mutex
	discoveries := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var response any
		switch r.URL.Path {
		case "/repos/acme/orders/installation", "/repos/other/reports/installation":
			if len(strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")) != 3 {
				t.Error("installation discovery did not use App JWT")
			}
			discoveries[r.URL.Path]++
			id := 1
			if strings.Contains(r.URL.Path, "other") {
				id = 2
			}
			response = map[string]int{"id": id}
		case "/app/installations/1/access_tokens":
			response = map[string]any{"token": "tenant-one", "expires_at": time.Now().Add(time.Hour)}
		case "/app/installations/2/access_tokens":
			response = map[string]any{"token": "tenant-two", "expires_at": time.Now().Add(time.Hour)}
		case "/repos/acme/orders/issues/12":
			if r.Header.Get("Authorization") != "Bearer tenant-one" {
				t.Error("wrong installation used for acme")
			}
			response = map[string]int{"number": 12}
		case "/repos/other/reports/issues/12":
			if r.Header.Get("Authorization") != "Bearer tenant-two" {
				t.Error("wrong installation used for other")
			}
			response = map[string]int{"number": 12}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	transport := NewAppTransport(connectorgithub.InstallationTokenConfig{Endpoint: server.URL + "/graphql", AppID: "123", PrivateKey: privateKey, HTTPClient: server.Client()})
	for _, repository := range []string{"acme/orders", "other/reports", "acme/orders"} {
		t.Run(repository, func(t *testing.T) {
			var issue struct {
				Number int `json:"number"`
			}
			if err := transport.REST(scopedRequests(t.Context(), "native", "intake"), http.MethodGet, "/repos/"+repository+"/issues/12", nil, &issue); err != nil {
				t.Fatal(err)
			}
			if issue.Number != 12 {
				t.Fatalf("issue number = %d", issue.Number)
			}
		})
	}
	if discoveries["/repos/acme/orders/installation"] != 1 || discoveries["/repos/other/reports/installation"] != 1 {
		t.Fatalf("discovery repeated or crossed tenants: %+v", discoveries)
	}
}
