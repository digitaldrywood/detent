package spritesigning

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

type memoryStore struct {
	rows map[string]Ownership
	fail bool
}

func (s *memoryStore) Load(_ context.Context, member string) (Ownership, bool, error) {
	row, exists := s.rows[member]
	return row, exists, nil
}

func (s *memoryStore) Save(_ context.Context, row Ownership) error {
	if s.fail {
		return errors.New("fixture write failed")
	}
	s.rows[row.Member] = row
	return nil
}

type fakeGitHub struct {
	account         Account
	keys            map[int64]map[int64]string
	creates         int
	deletes         []int64
	uncertainCreate bool
	uncertainDelete bool
	noAuthority     bool
}

func (g *fakeGitHub) Account(context.Context) (Account, error) {
	if g.noAuthority {
		return Account{}, ErrAuthority
	}
	return g.account, nil
}

func (g *fakeGitHub) Find(_ context.Context, account int64, public string) (Key, bool, error) {
	for id, candidate := range g.keys[account] {
		if candidate == public {
			return Key{ID: id, Public: public}, true, nil
		}
	}
	return Key{}, false, nil
}

func (g *fakeGitHub) Create(_ context.Context, account int64, public string) (Key, WriteOutcome, error) {
	g.creates++
	key := Key{ID: int64(g.creates), Public: public}
	if g.keys[account] == nil {
		g.keys[account] = map[int64]string{}
	}
	g.keys[account][key.ID] = public
	if g.uncertainCreate {
		return Key{}, Uncertain, errors.New("fixture connection lost")
	}
	return key, Committed, nil
}

func (g *fakeGitHub) Delete(_ context.Context, account, id int64) (WriteOutcome, error) {
	g.deletes = append(g.deletes, id)
	if g.uncertainDelete {
		return Uncertain, errors.New("fixture connection lost")
	}
	delete(g.keys[account], id)
	return Committed, nil
}

type fakeIdentity struct {
	public    string
	verifyErr error
	verified  bool
}

func (i *fakeIdentity) PublicKey(context.Context) (string, error) { return i.public, nil }
func (i *fakeIdentity) ConfigureAndVerify(context.Context, Account) error {
	i.verified = i.verifyErr == nil
	return i.verifyErr
}

func TestSetupPreservesAccountAndMemberOwnership(t *testing.T) {
	ctx := t.Context()
	store := &memoryStore{rows: map[string]Ownership{}}
	github := &fakeGitHub{keys: map[int64]map[int64]string{1: {99: "unrelated"}, 2: {99: "unrelated"}}}
	lifecycle := Lifecycle{GitHub: github, Store: store}
	for _, test := range []struct {
		member  string
		account int64
		public  string
	}{
		{"first", 1, "first-public"}, {"second", 1, "second-public"}, {"third", 2, "third-public"}, {"fourth", 2, "fourth-public"},
	} {
		t.Run(test.member, func(t *testing.T) {
			github.account = Account{ID: test.account, Login: "example", Name: "Example", Email: "example@example.test"}
			identity := &fakeIdentity{public: test.public}
			for range 2 {
				if err := lifecycle.Setup(ctx, test.member, identity); err != nil {
					t.Fatal(err)
				}
			}
			owned := store.rows[test.member]
			if !identity.verified || owned.AccountID != test.account || owned.KeyID <= 0 || github.keys[test.account][owned.KeyID] != test.public {
				t.Fatalf("ownership = %+v, verified = %v", owned, identity.verified)
			}
			if err := lifecycle.Retire(ctx, test.member, false); !errors.Is(err, ErrWorkHeld) || len(github.deletes) != 0 {
				t.Fatalf("active cleanup: %v", err)
			}
		})
	}
	if github.creates != 4 {
		t.Fatalf("created %d keys, want 4", github.creates)
	}
	github.account.ID = 1
	if err := lifecycle.Setup(ctx, "first", &fakeIdentity{public: "replacement"}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("replaced identity: %v", err)
	}
	github.account.ID = 2
	if err := lifecycle.Retire(ctx, "first", true); !errors.Is(err, ErrOwnership) {
		t.Fatalf("cross-account cleanup: %v", err)
	}
	github.account.ID = 1
	if err := lifecycle.Retire(ctx, "first", true); err != nil {
		t.Fatal(err)
	}
	if len(github.deletes) != 1 || !store.rows["first"].Removed || github.keys[1][99] != "unrelated" || len(github.keys[1]) != 2 || len(github.keys[2]) != 3 {
		t.Fatal("cleanup changed unrelated ownership")
	}
}

func TestUncertainRegistrationAndCleanupRetainOwnership(t *testing.T) {
	for _, scenario := range []string{"registration", "deletion", "already removed", "authority unavailable", "verification failed", "persistence failed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			store := &memoryStore{rows: map[string]Ownership{}}
			github := &fakeGitHub{account: Account{ID: 1, Login: "example", Name: "Example", Email: "example@example.test"}, keys: map[int64]map[int64]string{}}
			lifecycle := Lifecycle{GitHub: github, Store: store}
			identity := &fakeIdentity{public: "member-public"}
			switch scenario {
			case "registration":
				github.uncertainCreate = true
			case "verification failed":
				identity.verifyErr = errors.New("fixture member unreachable")
			case "persistence failed":
				store.fail = true
			}
			err := lifecycle.Setup(ctx, "member", identity)
			if scenario == "persistence failed" {
				if err == nil || github.creates != 0 {
					t.Fatalf("unowned registration: %v", err)
				}
				return
			}
			if scenario == "registration" || scenario == "verification failed" {
				if err == nil || !store.rows["member"].RegistrationAttempted {
					t.Fatalf("lost setup ownership: %v", err)
				}
				github.uncertainCreate = false
				identity.verifyErr = nil
				if err := lifecycle.Setup(ctx, "member", identity); err != nil {
					t.Fatal(err)
				}
				if github.creates != 1 {
					t.Fatal("duplicated uncertain registration")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "deletion":
				github.uncertainDelete = true
			case "already removed":
				delete(github.keys[1], store.rows["member"].KeyID)
			case "authority unavailable":
				github.noAuthority = true
			}
			err = lifecycle.Retire(ctx, "member", true)
			if scenario == "deletion" || scenario == "authority unavailable" {
				if err == nil || store.rows["member"].Removed {
					t.Fatalf("false cleanup success: %v", err)
				}
				github.uncertainDelete, github.noAuthority = false, false
				if err := lifecycle.Retire(ctx, "member", true); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !store.rows["member"].Removed || len(github.keys[1]) != 0 {
				t.Fatal("cleanup did not account for owned key")
			}
		})
	}
}

func TestLocalIdentitySignsAndVerifiesWithoutOverwritingUserKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "global"))
	t.Setenv("SSH_AUTH_SOCK", "")
	var publicKeys []string
	for _, member := range []string{"first", "second"} {
		identity := LocalIdentity{Directory: filepath.Join(home, member), Config: filepath.Join(home, member+".gitconfig")}
		public, err := identity.PublicKey(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		repeated, err := identity.PublicKey(t.Context())
		if err != nil || repeated != public {
			t.Fatalf("identity changed on retry: %v", err)
		}
		publicKeys = append(publicKeys, public)
		account := Account{ID: 1, Login: "example", Name: "Example User", Email: "123+example@users.noreply.github.com"}
		if err := identity.ConfigureAndVerify(t.Context(), account); err != nil {
			t.Fatal(err)
		}
		got, err := localCommand(t.Context(), "", "git", "config", "--file", identity.Config, "--get", "user.email")
		if err != nil || strings.TrimSpace(got) != account.Email {
			t.Fatalf("configured identity: %q, %v", got, err)
		}
		if err := os.WriteFile(identity.Config, []byte("[user]\n signingkey = user-managed\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := identity.ConfigureAndVerify(t.Context(), account); !errors.Is(err, ErrOwnership) {
			t.Fatalf("overwrote user key: %v", err)
		}
		got, err = localCommand(t.Context(), "", "git", "config", "--file", identity.Config, "--get", "user.signingkey")
		if err != nil || strings.TrimSpace(got) != "user-managed" {
			t.Fatalf("user key changed: %q, %v", got, err)
		}
	}
	if publicKeys[0] == publicKeys[1] {
		t.Fatal("members share signing identity")
	}
}
