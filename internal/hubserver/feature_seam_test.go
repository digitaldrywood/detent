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
