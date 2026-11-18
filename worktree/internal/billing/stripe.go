package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const StripeAPIVersion = "2025-06-30.basil"

const (
	ModeTest = "test"
	ModeLive = "live"
)

type StripeConfig struct {
	APIKey string
	Mode   string
	Client *http.Client
}

type stripeProvider struct {
	key    string
	live   bool
	client *http.Client
}

type StripeError struct {
	StatusCode int
	Method     string
	Path       string
	Type       string
	Code       string
	Param      string
	Message    string
	RequestID  string
	cause      error
}

func (e *StripeError) Error() string {
	return fmt.Sprintf("stripe billing request failed with status %d", e.StatusCode)
}

func (e *StripeError) Unwrap() error { return e.cause }

func ValidMode(mode string) bool {
	return mode == ModeTest || mode == ModeLive
}

func NewStripe(config StripeConfig) (Provider, error) {
	if config.Mode == "" {
		config.Mode = ModeTest
	}
	if !ValidMode(config.Mode) {
		return nil, errors.New("stripe billing mode must be test or live")
	}
	if (!strings.HasPrefix(config.APIKey, "sk_"+config.Mode+"_") && !strings.HasPrefix(config.APIKey, "rk_"+config.Mode+"_")) || len(config.APIKey) < 16 {
		return nil, errors.New("stripe billing requires a secret key matching the configured " + config.Mode + " mode")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	if config.Client != nil {
		*client = *config.Client
		client.Timeout = 15 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &stripeProvider{key: config.APIKey, live: config.Mode == ModeLive, client: client}, nil
}

func (s *stripeProvider) request(ctx context.Context, method, path, key string, form url.Values, result any) error {
	endpoint := "https://api.stripe.com/v1/" + path
	var body io.Reader
	if method == http.MethodGet {
		endpoint += "?" + form.Encode()
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("construct stripe request: %w", err)
	}
	req.SetBasicAuth(s.key, "")
	req.Header.Set("Stripe-Version", StripeAPIVersion)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("send stripe request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failure := &StripeError{StatusCode: response.StatusCode, Method: method, Path: path, RequestID: response.Header.Get("Request-Id")}
		var payload struct {
			Error struct{ Type, Code, Param, Message string }
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
		if readErr != nil {
			failure.cause = fmt.Errorf("read stripe error response: %w", readErr)
		} else if len(raw) <= 2*1024*1024 && json.Unmarshal(raw, &payload) == nil {
			failure.Type, failure.Code, failure.Param, failure.Message = payload.Error.Type, payload.Error.Code, payload.Error.Param, payload.Error.Message
		}
		if response.StatusCode == http.StatusPaymentRequired && path == "payment_intents" {
			failure.cause = errors.Join(failure.cause, ErrPaymentFailed)
		}
		return failure
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(raw) > 2*1024*1024 || json.Unmarshal(raw, result) != nil {
		return errors.New("stripe returned an invalid billing response")
	}
	return nil
}

func (s *stripeProvider) verifyBinding(ctx context.Context, binding Binding) error {
	if !validID(binding.AccountID, "acct_") || !validID(binding.CustomerID, "cus_") || !validID(binding.OrganizationID, "org_") {
		return errors.New("stripe organization binding is invalid")
	}
	var account struct{ ID string }
	if err := s.request(ctx, http.MethodGet, "account", "", nil, &account); err != nil {
		return err
	}
	if account.ID != binding.AccountID {
		return errors.New("stripe account does not match the configured organization binding")
	}
	var customer struct {
		ID       string
		Livemode *bool
		Deleted  bool
		Metadata map[string]string
	}
	if err := s.request(ctx, http.MethodGet, "customers/"+binding.CustomerID, "", nil, &customer); err != nil {
		return err
	}
	if customer.ID != binding.CustomerID || !s.mode(customer.Livemode) || customer.Deleted || customer.Metadata["detent_organization_id"] != binding.OrganizationID {
		return errors.New("stripe customer does not match the configured organization and mode")
	}
	return nil
}

func validID(id, prefix string) bool {
	return strings.HasPrefix(id, prefix) && len(id) > len(prefix) && len(id) <= 128 && strings.IndexFunc(id, func(r rune) bool {
		return r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}) == -1
}

func modeMatches(livemode *bool, live bool) bool {
	return livemode != nil && *livemode == live
}

func (s *stripeProvider) sessionPrefix() string {
	if s.live {
		return "cs_live_"
	}
	return "cs_test_"
}

func (s *stripeProvider) mode(livemode *bool) bool {
	return modeMatches(livemode, s.live)
}

func sessionURL(value, host string) bool {
	u, err := url.Parse(value)
	return err == nil && u != nil && u.Scheme == "https" && u.Host == host && u.User == nil
}
