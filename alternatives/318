package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/serviceapi"
)

func TestAuditEvidenceResultOmitsCredentialsAndRawFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	result := auditEvidenceResultFromRun(securityaudit.Run{
		ID:                 7,
		InvocationID:       "invocation-7",
		ProjectID:          "detent",
		Repository:         "digitaldrywood/detent",
		PRNumber:           2006,
		BaseSHA:            "base-7",
		HeadSHA:            "head-7",
		ServiceIdentity:    "detent:detent",
		ReviewerVersion:    securityaudit.ReviewerVersion,
		ReviewerDigest:     securityaudit.ReviewerDigest(),
		AuthenticationMode: securityaudit.AuthenticationSubscription,
		WorkerPID:          987654,
		ProviderThreadID:   "private-provider-thread",
		ExitStatus:         securityaudit.ExitStatusFailed,
		Failure:            "credential=must-not-be-exposed",
		OutputDigest:       securityaudit.OutputDigest("output"),
		OutputBytes:        6,
		Verdict:            securityaudit.VerdictFail,
		Summary:            "One finding.",
		Findings:           []securityaudit.Finding{{ID: "auth-1", Severity: "p1", Body: "Missing authorization."}},
		Attempt:            1,
		StartedAt:          now,
		CompletedAt:        now.Add(time.Second),
		RecordedAt:         now.Add(time.Second),
	}, nil)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	encoded := string(raw)
	for _, forbidden := range []string{"must-not-be-exposed", "private-provider-thread", "987654"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("audit evidence exposes %q: %s", forbidden, encoded)
		}
	}
	for _, required := range []string{"invocation-7", "base-7", "head-7", securityaudit.AuthenticationSubscription} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("audit evidence missing %q: %s", required, encoded)
		}
	}
}

func TestAuditEvidenceResultShowsOversizedDiff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		failure    string
		wantReason string
	}{
		{name: "oversized", failure: (&securityaudit.DiffTooLargeError{ActualBytes: 272414, LimitBytes: 262144}).Error(), wantReason: securityaudit.ReasonDiffTooLarge},
		{name: "other failure stays private", failure: "credential=private"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := auditEvidenceResultFromRun(securityaudit.Run{ExitStatus: securityaudit.ExitStatusFailed, Failure: tc.failure, AuthenticationMode: securityaudit.AuthenticationNotRun}, nil)
			if result.FailureReason != tc.wantReason || result.AuthenticationMode != securityaudit.AuthenticationNotRun {
				t.Fatalf("result = %#v, want reason %q", result, tc.wantReason)
			}
			if tc.wantReason == securityaudit.ReasonDiffTooLarge && (result.ActualBytes != 272414 || result.MaxDiffBytes != 262144) {
				t.Fatalf("result = %#v, want actual and limit", result)
			}
			raw, err := json.Marshal(result)
			if err != nil || strings.Contains(string(raw), tc.failure) {
				t.Fatalf("evidence = %s, error = %v; raw failure leaked", raw, err)
			}
		})
	}
}

func TestAuditEvidenceRequiresBaseAndHeadTogether(t *testing.T) {
	t.Parallel()

	configPath := ""
	cmd := newAuditEvidenceCommand(&configPath, defaultOptions())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--project", "detent",
		"--repository", "digitaldrywood/detent",
		"--pull-request", "2006",
		"--base-sha", "base-7",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--base-sha and --head-sha") {
		t.Fatalf("Execute() error = %v, want exact SHA pair validation", err)
	}
}

func TestAuditCommandsRequireHubWithoutRuntimeFallback(t *testing.T) {
	for _, test := range []struct {
		name           string
		serviceAddress string
		host           string
		port           int
		portSet        bool
	}{
		{name: "no Hub configuration"},
		{name: "local service credentials cannot authorize audit", serviceAddress: "example.invalid:1234", host: "example.invalid", port: 1234, portSet: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := dashboardAddressOptions(globalconfig.Config{APIToken: "example"})
			opts.lookupEnv = func(name string) string {
				switch name {
				case "DETENT_SERVICE_ADDRESS":
					return test.serviceAddress
				case serviceapi.DispositionTokenEnvironment:
					return "example-service-token"
				default:
					return ""
				}
			}
			opts.httpDo = func(*http.Request) (*http.Response, error) {
				t.Fatal("audit command attempted local service fallback")
				return nil, nil
			}
			key := securityaudit.Key{ProjectID: "example", Repository: "example-org/example-project", PRNumber: 1}
			if _, err := loadAuditEvidence(t.Context(), "/config/global.yaml", key, opts); err == nil || !strings.Contains(err.Error(), "require configured Hub") {
				t.Fatalf("audit read did not require Hub: %v", err)
			}
			if _, err := runAuditDisposition(t.Context(), "/config/global.yaml", test.host, test.port, test.portSet, "example", key.Repository, 1, "base", "head", "finding", "evidence", opts); err == nil || !strings.Contains(err.Error(), "require configured Hub") {
				t.Fatalf("audit write did not require Hub: %v", err)
			}
		})
	}
}

func TestRunAuditDispositionUsesHubWithoutLocalService(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/api/v2/organizations/org_example/projects/prj_example/security-audits/dispositions" || r.Header.Get("Authorization") != "Bearer example-token" {
					t.Errorf("unexpected audit authority: %s", r.URL.Path)
					http.Error(w, "unexpected authority", http.StatusBadRequest)
					return
				}
				var command securityaudit.DispositionCommand
				if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
					t.Error(err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if command.Key.ProjectID != "prj_example" || command.Key.HeadSHA != "head" || command.FindingID != "finding" || !command.Confirm || command.RequestID == "" {
					t.Errorf("audit source was not preserved: %+v", command)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status == http.StatusOK {
					_ = json.NewEncoder(w).Encode(securityaudit.Disposition{ID: 9, AuditRunID: 7, FindingID: "finding"})
				} else {
					_, _ = w.Write([]byte(`{"code":"unavailable","message":"Hub unavailable"}`))
				}
			}))
			defer server.Close()
			opts := dashboardAddressOptions(globalconfig.Config{Client: globalconfig.HubClient{URL: server.URL, OrganizationID: "org_example", NativeProjects: map[string]string{"local": "prj_example"}, TokenEnvironment: "EXAMPLE_HUB_TOKEN"}})
			opts.lookupEnv = func(key string) string {
				if key == "EXAMPLE_HUB_TOKEN" {
					return "example-token"
				}
				return ""
			}
			opts.httpDo = server.Client().Do
			result, err := runAuditDisposition(t.Context(), "/config/runner.yaml", "127.0.0.1", 0, false, "local", "example-org/example-project", 7, "base", "head", "finding", "Verified source evidence", opts)
			if (err == nil) != (status == http.StatusOK) || calls.Load() != 1 {
				t.Fatalf("Hub audit result=%+v error=%v calls=%d", result, err, calls.Load())
			}
			if err == nil && result.ID != 9 {
				t.Fatalf("Hub disposition receipt was lost: %+v", result)
			}
		})
	}
}

func nonLoopbackListener(t *testing.T) net.Listener {
	t.Helper()

	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces() error = %v", err)
	}
	for _, networkInterface := range interfaces {
		if networkInterface.Flags&net.FlagUp == 0 || networkInterface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil || ip.IsLoopback() || ip.IsUnspecified() || ip.To4() == nil {
				continue
			}
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", net.JoinHostPort(ip.String(), "0"))
			if err == nil {
				return listener
			}
		}
	}
	t.Skip("host has no bindable non-loopback IPv4 address")
	return nil
}

func TestAuditDispositionRequiresConfirmation(t *testing.T) {
	t.Parallel()

	configPath := ""
	host := ""
	port := -1
	cmd := newAuditDispositionCommand(&configPath, &host, &port, defaultOptions())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--project", "detent",
		"--repository", "digitaldrywood/detent",
		"--pull-request", "2006",
		"--base-sha", "base-7",
		"--head-sha", "head-7",
		"--finding", "auth-1",
		"--evidence", "Verified administrator-only path.",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "requires --yes") {
		t.Fatalf("Execute() error = %v, want confirmation validation", err)
	}
}
