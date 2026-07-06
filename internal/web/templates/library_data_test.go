package templates

import "testing"

func TestLibrarySafeURLValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "https", in: "https://github.com/digitaldrywood/detent/pull/1", want: "https://github.com/digitaldrywood/detent/pull/1"},
		{name: "http", in: "http://127.0.0.1:8080/review/ad-1", want: "http://127.0.0.1:8080/review/ad-1"},
		{name: "root relative", in: "/review/ad-1", want: "/review/ad-1"},
		{name: "javascript", in: "javascript:alert(1)", want: ""},
		{name: "protocol relative", in: "//example.com/review", want: ""},
		{name: "mailto", in: "mailto:team@example.com", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := librarySafeURLValue(tt.in); got != tt.want {
				t.Fatalf("librarySafeURLValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
