package spritesigning

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrAuthority = errors.New("GitHub user signing-key authority is unavailable")
	ErrPending   = errors.New("signing-key registration outcome requires reconciliation")
	ErrWorkHeld  = errors.New("member still owns unpublished work")
	ErrOwnership = errors.New("signing-key ownership does not match")
	ErrRejected  = errors.New("signing-key operation was rejected")
)

type Account struct {
	ID    int64
	Login string
	Name  string
	Email string
}

type Key struct {
	ID     int64
	Public string
}

type WriteOutcome uint8

const (
	Uncertain WriteOutcome = iota
	Rejected
	Committed
)

type GitHub interface {
	Account(context.Context) (Account, error)
	Find(context.Context, int64, string) (Key, bool, error)
	Create(context.Context, int64, string) (Key, WriteOutcome, error)
	Delete(context.Context, int64, int64) (WriteOutcome, error)
}

type Ownership struct {
	Member                string
	AccountID             int64
	PublicKey             string
	KeyID                 int64
	RegistrationAttempted bool
	Removed               bool
}

type Store interface {
	Load(context.Context, string) (Ownership, bool, error)
	Save(context.Context, Ownership) error
}

type Identity interface {
	PublicKey(context.Context) (string, error)
	ConfigureAndVerify(context.Context, Account) error
}

type Lifecycle struct {
	GitHub GitHub
	Store  Store
}

func (l Lifecycle) Setup(ctx context.Context, member string, identity Identity) error {
	account, err := l.GitHub.Account(ctx)
	if err != nil {
		return fmt.Errorf("resolve signing account: %w", err)
	}
	if member == "" || account.ID <= 0 || account.Login == "" || account.Name == "" || account.Email == "" {
		return ErrOwnership
	}
	public, err := identity.PublicKey(ctx)
	if err != nil {
		return fmt.Errorf("prepare member signing identity: %w", err)
	}
	if public == "" {
		return ErrOwnership
	}
	owned, exists, err := l.Store.Load(ctx, member)
	if err != nil {
		return fmt.Errorf("load signing ownership: %w", err)
	}
	if exists && (owned.Member != member || owned.AccountID != account.ID || owned.PublicKey != public || owned.Removed) {
		return ErrOwnership
	}
	if !exists {
		owned = Ownership{Member: member, AccountID: account.ID, PublicKey: public}
		if err := l.Store.Save(ctx, owned); err != nil {
			return fmt.Errorf("reserve signing ownership: %w", err)
		}
	}
	key, found, err := l.GitHub.Find(ctx, owned.AccountID, public)
	if err != nil {
		return fmt.Errorf("reconcile signing key: %w", err)
	}
	if found && (!owned.RegistrationAttempted || key.ID <= 0 || key.Public != public || owned.KeyID != 0 && owned.KeyID != key.ID) {
		return ErrOwnership
	}
	if !found {
		if owned.KeyID != 0 {
			return ErrOwnership
		}
		owned.RegistrationAttempted = true
		if err := l.Store.Save(ctx, owned); err != nil {
			return fmt.Errorf("record signing registration intent: %w", err)
		}
		var outcome WriteOutcome
		key, outcome, err = l.GitHub.Create(ctx, account.ID, public)
		if outcome != Committed || err != nil {
			if outcome == Rejected {
				owned.RegistrationAttempted = false
				return errors.Join(ErrRejected, err, l.Store.Save(ctx, owned))
			}
			return errors.Join(ErrPending, err)
		}
		if key.ID <= 0 || key.Public != public {
			return ErrPending
		}
	}
	owned.KeyID = key.ID
	if err := l.Store.Save(ctx, owned); err != nil {
		return fmt.Errorf("record registered signing key: %w", err)
	}
	if err := identity.ConfigureAndVerify(ctx, account); err != nil {
		return fmt.Errorf("verify member signing: %w", err)
	}
	return nil
}

func (l Lifecycle) Retire(ctx context.Context, member string, publicationDurable bool) error {
	if !publicationDurable {
		return ErrWorkHeld
	}
	owned, exists, err := l.Store.Load(ctx, member)
	if err != nil {
		return fmt.Errorf("load signing cleanup ownership: %w", err)
	}
	if !exists || owned.Removed {
		return nil
	}
	if owned.Member != member || owned.AccountID <= 0 || owned.PublicKey == "" {
		return ErrOwnership
	}
	account, err := l.GitHub.Account(ctx)
	if err != nil {
		return fmt.Errorf("resolve signing cleanup authority: %w", err)
	}
	if account.ID != owned.AccountID {
		return ErrOwnership
	}
	key, found, err := l.GitHub.Find(ctx, owned.AccountID, owned.PublicKey)
	if err != nil {
		return fmt.Errorf("reconcile signing cleanup: %w", err)
	}
	if found {
		if !owned.RegistrationAttempted || key.ID <= 0 || key.Public != owned.PublicKey || owned.KeyID != 0 && key.ID != owned.KeyID {
			return ErrOwnership
		}
		owned.KeyID = key.ID
		if err := l.Store.Save(ctx, owned); err != nil {
			return fmt.Errorf("record signing cleanup target: %w", err)
		}
		outcome, err := l.GitHub.Delete(ctx, owned.AccountID, owned.KeyID)
		if outcome != Committed || err != nil {
			return errors.Join(ErrPending, err)
		}
	}
	owned.Removed = true
	if err := l.Store.Save(ctx, owned); err != nil {
		return fmt.Errorf("record signing cleanup: %w", err)
	}
	return nil
}
