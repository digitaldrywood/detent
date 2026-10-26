package runnerauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsCredentialDACL(t *testing.T) {
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
			setTestDACL(t, directory, "D:P(A;OICI;FA;;;"+sid+")(A;OICI;GR;;;WD)", true)
			identityPath := filepath.Join(directory, "identity.json")
			identity, err := Initialize(identityPath, "https://hub.example.test")
			if err != nil {
				t.Fatal(err)
			}
			path := identityPath
			load := func() error {
				loaded, err := Load(identityPath)
				if err == nil && (loaded.Credential != identity.Credential || loaded.Identity.Binding != identity.Identity.Binding || loaded.HubURL != identity.HubURL) {
					t.Fatal("loaded identity changed its credential or binding")
				}
				return err
			}
			write := func() error { return Save(identityPath, identity) }
			if kind == "routing cache" {
				path = RoutingCachePath(identityPath)
				snapshot := RoutingSnapshot{RunnerID: identity.Identity.RunnerID, Revision: 1, Routing: Routing{DisplayName: "Runner", State: "active", CapacityLimit: 1}}
				write = func() error { return SaveRoutingCache(identityPath, snapshot) }
				load = func() error {
					loaded, err := LoadRoutingCache(identityPath)
					if err == nil && (loaded.RunnerID != snapshot.RunnerID || loaded.Revision != snapshot.Revision || loaded.Routing.DisplayName != snapshot.Routing.DisplayName) {
						t.Fatal("loaded routing cache changed its snapshot")
					}
					return err
				}
				if err := write(); err != nil {
					t.Fatal(err)
				}
			}
			assertUserOnlyDACL(t, path, user.User.Sid)
			if err := load(); err != nil {
				t.Fatalf("fresh credential could not be loaded: %v", err)
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
			if err != nil {
				t.Fatal(err)
			}
			control, _, err := sd.Control()
			if err != nil || control&windows.SE_DACL_PROTECTED != 0 {
				t.Fatalf("legacy fixture must inherit its DACL: control=%x err=%v", control, err)
			}
			if err := load(); err != nil {
				t.Fatalf("legacy credential could not be loaded at startup: %v", err)
			}
			assertUserOnlyDACL(t, path, user.User.Sid)
			for _, test := range []struct {
				name, sddl string
				protected  bool
			}{
				{"extra ACE", "D:P(A;;FA;;;" + sid + ")(A;;GR;;;WD)", true},
				{"read-only ACE", "D:P(A;;GR;;;" + sid + ")", true},
			} {
				t.Run(test.name, func(t *testing.T) {
					setTestDACL(t, path, test.sddl, test.protected)
					if err := load(); err == nil || !strings.Contains(err.Error(), "icacls") {
						t.Fatalf("unsafe DACL needs an actionable read error, got %v", err)
					}
					if err := write(); err != nil {
						t.Fatalf("legacy credential could not be resecured: %v", err)
					}
					assertUserOnlyDACL(t, path, user.User.Sid)
					if err := load(); err != nil {
						t.Fatalf("resecured credential could not be loaded: %v", err)
					}
				})
			}
		})
	}
}

func setTestDACL(t *testing.T, path, sddl string, protected bool) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	information := windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	if protected {
		information = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.SECURITY_INFORMATION(information), nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsCredentialOwner(t *testing.T) {
	t.Parallel()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	for _, test := range []struct {
		name, owner string
		valid       bool
	}{
		{"current user", "O:" + sid, true},
		{"foreign owner", "O:S-1-5-21-1-2-3-1001", false},
		{"missing owner", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			sd, err := windows.SecurityDescriptorFromString(test.owner + "D:P(A;;FA;;;" + sid + ")")
			if err != nil {
				t.Fatal(err)
			}
			err = validatePrivateSecurityDescriptor(sd)
			if test.valid {
				if err != nil {
					t.Fatalf("current-user ownership should be accepted: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "set the owner") || !strings.Contains(err.Error(), "icacls") {
				t.Fatalf("unsafe ownership needs an actionable read error, got %v", err)
			}
		})
	}
}

func assertUserOnlyDACL(t *testing.T, path string, sid *windows.SID) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(sid) {
		t.Fatalf("credential must be owned by the current user: owner=%v err=%v", owner, err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL inheritance is not disabled: control=%x err=%v", control, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("DACL must have exactly one ACE: %v err=%v", dacl, err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != 0x1f01ff || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
		t.Fatalf("ACE must grant full control only to the current user: %v", ace)
	}
}
