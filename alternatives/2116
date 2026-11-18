package hubapi_test

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func TestFromFeatureError(t *testing.T) {
	t.Parallel()
	plain := errors.New("plain failure")
	detailed := &hubapi.Error{Code: "limit_reached", Message: "Limit reached", CurrentRevision: 9, Details: map[string]any{"limit": 3}, Status: http.StatusTooManyRequests}
	for _, test := range []struct {
		name string
		err  error
		want *hubapi.NativeError
	}{
		{"invalid", hubapi.Invalid("title is required"), hubapi.NativeInvalid("title is required").(*hubapi.NativeError)},
		{"public invalid", hubapi.PublicInvalid("title is required"), hubapi.NativePublicInvalid("title is required").(*hubapi.NativeError)},
		{"conflict", hubapi.Conflict(4), hubapi.NativeConflict(4).(*hubapi.NativeError)},
		{"wrapped conflict", fmt.Errorf("update thread: %w", hubapi.Conflict(5)), hubapi.NativeConflict(5).(*hubapi.NativeError)},
		{"details and revision", detailed, &hubapi.NativeError{Code: "limit_reached", Message: "Limit reached", CurrentRevision: 9, Details: map[string]any{"limit": 3}, Status: http.StatusTooManyRequests}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var got *hubapi.NativeError
			if !errors.As(hubapi.FromFeatureError(test.err), &got) {
				t.Fatalf("fromFeatureError(%v) is not a nativeError", test.err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("mapped = %+v, want %+v", got, test.want)
			}
		})
	}
	var notFound *hubapi.NativeError
	if !errors.As(hubapi.FromFeatureError(hubapi.NotFound()), &notFound) || !hubapi.IsNativeNotFound(notFound) || notFound.Status != http.StatusNotFound || !notFound.PublicMessage || len(notFound.LogAttributes) != 2 || notFound.LogAttributes[0] != "not_found_source" {
		t.Fatalf("mapped not found = %+v", notFound)
	}
	if got := hubapi.FromFeatureError(plain); got != plain {
		t.Fatalf("plain error mapped to %v", got)
	}
	if got := hubapi.FromFeatureError(nil); got != nil {
		t.Fatalf("nil mapped to %v", got)
	}
}

func TestOperatorSourceRefusal(t *testing.T) {
	t.Parallel()
	private := "source bundle must advertise only the reviewed head; sentinel-private-source"
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"version validation", hubapi.Rejection(hubapi.Invalid(private), "version_validation", "source"), true},
		{"native version validation", fmt.Errorf("publish: %w", hubapi.FromFeatureError(hubapi.Rejection(hubapi.Invalid(private), "version_validation", "source"))), true},
		{"source verification", hubapi.FromFeatureError(hubapi.Rejection(hubapi.Invalid(private), "source_verification", "source")), true},
		{"policy validation", hubapi.Rejection(hubapi.Invalid(private), "version_validation", "policy_id"), false},
		{"untyped invalid", hubapi.NativeInvalid(private), false},
		{"conflict", hubapi.NativeConflict(3), false},
		{"plain", errors.New(private), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			refusal, ok := hubapi.OperatorSourceRefusal(test.err)
			if ok != test.want {
				t.Fatalf("source refusal = %v, want %v", ok, test.want)
			}
			if !ok {
				return
			}
			if refusal.Code != "invalid_request" || refusal.Message != hubapi.SourceRefusalGuidance {
				t.Fatalf("refusal = %+v", refusal)
			}
		})
	}
	for _, phrase := range []string{"git bundle create", "verification budget"} {
		if !strings.Contains(hubapi.SourceRefusalGuidance, phrase) {
			t.Fatalf("guidance lacks %q", phrase)
		}
	}
}
