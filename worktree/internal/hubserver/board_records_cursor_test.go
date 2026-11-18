package hubserver

import (
	"net/url"
	"testing"
)

func TestNativeWorkCursorParamsIgnoreProjectionIncludes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		query string
		want  string
	}{
		{"no include", "state=Todo", "state=Todo"},
		{"work", "include=work&state=Todo", "state=Todo"},
		{"summary", "include=summary", ""},
		{"projection with other include", "include=summary,history", "include=history"},
		{"empty include", "include=", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			params, err := url.ParseQuery(test.query)
			if err != nil {
				t.Fatal(err)
			}
			if got := nativeWorkCursorParams(params).Encode(); got != test.want {
				t.Fatalf("cursor params = %q, want %q", got, test.want)
			}
		})
	}
}
