package runnerauth

import (
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateFiles(t *testing.T) {
	t.Parallel()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	for _, kind := range []string{"identity", "routing cache"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			setWindowsFileDACL(t, directory, "D:P(A;OICI;FA;;;"+sid+")(A;OICI;FR;;;WD)")
			identityPath := filepath.Join(directory, "identity.json")
			identity, err := Initialize(identityPath, "https://hub.example.test")
			if err != nil {
				t.Fatal(err)
			}
			path := identityPath
			write := func() error { return Save(identityPath, identity) }
			read := func() error {
				loaded, err := Load(identityPath)
				if err == nil && loaded.Credential != identity.Credential {
					t.Fatal("credential changed during ACL update")
				}
				return err
			}
			if kind == "routing cache" {
				path = RoutingCachePath(identityPath)
				snapshot := RoutingSnapshot{RunnerID: identity.Identity.RunnerID, Revision: 3, Routing: Routing{
					DisplayName: "Runner", State: "active", CapacityLimit: 2, IsolationTier: "native-trusted",
				}}
				write = func() error { return SaveRoutingCache(identityPath, snapshot) }
				read = func() error {
					loaded, err := LoadRoutingCache(identityPath)
					if err == nil && (loaded.Revision != snapshot.Revision || loaded.RunnerID != snapshot.RunnerID) {
						t.Fatal("routing cache changed during ACL update")
					}
					return err
				}
				if err := write(); err != nil {
					t.Fatal(err)
				}
			}
			assertWindowsPrivateDACL(t, path, user.User.Sid)
			if err := read(); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name string
				sddl string
			}{
				{"extra account", "D:P(A;;FA;;;" + sid + ")(A;;FR;;;WD)"},
				{"inheritance enabled", "D:(A;;FA;;;" + sid + ")"},
				{"read only", "D:P(A;;FR;;;" + sid + ")"},
				{"null DACL", "D:NO_ACCESS_CONTROL"},
			} {
				t.Run(test.name, func(t *testing.T) {
					setWindowsFileDACL(t, path, test.sddl)
					if err := read(); err == nil || !strings.Contains(err.Error(), "protected DACL") || !strings.Contains(err.Error(), "disable inheritance") {
						t.Fatalf("unsafe DACL read error = %v", err)
					}
					if err := write(); err != nil {
						t.Fatalf("legacy file update: %v", err)
					}
					assertWindowsPrivateDACL(t, path, user.User.Sid)
					if err := read(); err != nil {
						t.Fatalf("read updated file: %v", err)
					}
				})
			}
		})
	}
}

func assertWindowsPrivateDACL(t *testing.T, path string, sid *windows.SID) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("file DACL is not protected with one ACE: %s", descriptor.String())
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != 0x1f01ff || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
		t.Fatalf("file DACL does not grant only current-user full control: %s", descriptor.String())
	}
}

func setWindowsFileDACL(t *testing.T, path, sddl string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	protection := windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	if control&windows.SE_DACL_PROTECTED != 0 || dacl == nil {
		protection = windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.SECURITY_INFORMATION(protection), nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}
