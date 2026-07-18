package procgroup

import (
	"reflect"
	"testing"
)

func TestEnvironmentWithTempDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		goos        string
		environment []string
		want        []string
	}{
		{
			name:        "unix replaces exact temp variables",
			goos:        "linux",
			environment: []string{"PATH=/bin", "TMPDIR=/host/tmp", "TMP=/host/tmp", "TEMP=/host/tmp", "tmp=/preserved"},
			want:        []string{"PATH=/bin", "tmp=/preserved", "TMPDIR=/workspace/.detent/tmp", "TMP=/workspace/.detent/tmp", "TEMP=/workspace/.detent/tmp"},
		},
		{
			name:        "windows replaces temp variables case insensitively",
			goos:        "windows",
			environment: []string{"Path=C:\\bin", "tmpdir=C:\\host", "Tmp=C:\\host", "temp=C:\\host"},
			want:        []string{"Path=C:\\bin", "TMPDIR=/workspace/.detent/tmp", "TMP=/workspace/.detent/tmp", "TEMP=/workspace/.detent/tmp"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := environmentWithTempDir(tt.environment, "/workspace/.detent/tmp", tt.goos)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("environmentWithTempDir() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestEnvironmentWithOverrides(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		goos        string
		environment []string
		configured  Environment
		want        []string
	}{
		{
			name:        "unix replaces variables and prepends tool paths",
			goos:        "linux",
			environment: []string{"PATH=/usr/bin", "GOCACHE=/host/cache", "HOME=/home/example"},
			configured: Environment{
				Variables:    map[string]string{"GOMODCACHE": "/shared/go-mod", "GOCACHE": "/shared/go-build"},
				PathPrefixes: []string{"/shared/go-bin", " "},
			},
			want: []string{"HOME=/home/example", "GOCACHE=/shared/go-build", "GOMODCACHE=/shared/go-mod", "PATH=/shared/go-bin:/usr/bin"},
		},
		{
			name:        "windows replaces variables case insensitively",
			goos:        "windows",
			environment: []string{"Path=C:\\Windows", "gocache=C:\\host", "TEMP=C:\\temp"},
			configured: Environment{
				Variables:    map[string]string{"GOCACHE": "D:\\shared\\go-build"},
				PathPrefixes: []string{"D:\\shared\\go-bin"},
			},
			want: []string{"TEMP=C:\\temp", "GOCACHE=D:\\shared\\go-build", "PATH=D:\\shared\\go-bin;C:\\Windows"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := environmentWithOverrides(tt.environment, tt.configured, tt.goos)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("environmentWithOverrides() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
