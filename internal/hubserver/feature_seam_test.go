package hubserver

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
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
		want *nativeError
	}{
		{"invalid", hubapi.Invalid("title is required"), nativeInvalid("title is required").(*nativeError)},
		{"public invalid", hubapi.PublicInvalid("title is required"), nativePublicInvalid("title is required").(*nativeError)},
		{"conflict", hubapi.Conflict(4), nativeConflict(4).(*nativeError)},
		{"wrapped conflict", fmt.Errorf("update thread: %w", hubapi.Conflict(5)), nativeConflict(5).(*nativeError)},
		{"details and revision", detailed, &nativeError{Code: "limit_reached", Message: "Limit reached", CurrentRevision: 9, Details: map[string]any{"limit": 3}, status: http.StatusTooManyRequests}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var got *nativeError
			if !errors.As(fromFeatureError(test.err), &got) {
				t.Fatalf("fromFeatureError(%v) is not a nativeError", test.err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("mapped = %+v, want %+v", got, test.want)
			}
		})
	}
	var notFound *nativeError
	if !errors.As(fromFeatureError(hubapi.NotFound()), &notFound) || !isNativeNotFound(notFound) || notFound.status != http.StatusNotFound || !notFound.publicMessage || len(notFound.logAttributes) != 2 || notFound.logAttributes[0] != "not_found_source" {
		t.Fatalf("mapped not found = %+v", notFound)
	}
	if got := fromFeatureError(plain); got != plain {
		t.Fatalf("plain error mapped to %v", got)
	}
	if got := fromFeatureError(nil); got != nil {
		t.Fatalf("nil mapped to %v", got)
	}
}
