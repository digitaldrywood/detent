package tracker

import "testing"

// The denylist of decisions section 18.4 is applied by path string, to both
// path and old_path, because a deleted or renamed file has no canonical target
// to resolve.
func TestDiffPathDenied(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "ordinary source file", path: "internal/tracker/native_diff.go"},
		{name: "git objects at the root", path: ".git/objects/ab/cdef", want: true},
		{name: "git objects nested", path: "vendor/thing/.git/objects/pack/x.pack", want: true},
		{name: "git config", path: ".git/config", want: true},
		{name: "git head is not denied", path: ".git/HEAD"},
		{name: "node modules segment", path: "web/node_modules/react/index.js", want: true},
		{name: "node modules as the leaf", path: "node_modules", want: true},
		{name: "a file merely named node_modules_backup", path: "node_modules_backup/file.js"},
		{name: "dot env", path: ".env", want: true},
		{name: "dot env suffixed", path: "config/.env.production", want: true},
		{name: "pem", path: "certs/server.pem", want: true},
		{name: "key", path: "deploy/tls.key", want: true},
		{name: "p12", path: "keystore.p12", want: true},
		{name: "id_rsa", path: "home/.ssh/id_rsa", want: true},
		{name: "id_rsa public", path: "home/.ssh/id_rsa.pub", want: true},
		{name: "uppercase secret", path: "certs/SERVER.PEM", want: true},
		{name: "windows separator", path: `web\node_modules\react\index.js`, want: true},
		{name: "dot segments are skipped", path: "./.git/config", want: true},
		{name: "empty", path: ""},
		{name: "a key in the name but not the suffix", path: "internal/keyring/store.go"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := DiffPathDenied(test.path); got != test.want {
				t.Fatalf("DiffPathDenied(%q) = %v, want %v", test.path, got, test.want)
			}
		})
	}
}
