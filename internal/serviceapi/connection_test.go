package serviceapi

import (
	"reflect"
	"testing"
)

func TestConnectionEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		connection Connection
		want       map[string]string
	}{
		{name: "unset"},
		{
			name:       "address and token",
			connection: Connection{Address: "100.111.222.33:4100", Token: "service-token"},
			want: map[string]string{
				AddressEnvironment: "100.111.222.33:4100",
				TokenEnvironment:   "service-token",
			},
		},
		{
			name:       "unauthenticated service clears ambient token",
			connection: Connection{Address: "127.0.0.1:4100"},
			want: map[string]string{
				AddressEnvironment: "127.0.0.1:4100",
				TokenEnvironment:   "",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.connection.Environment(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Connection.Environment() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
