package hubmaintenance

import (
	"context"
	"errors"
	"sync"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

var ErrByteBudget = errors.New("maintenance payload byte budget exhausted")

type budgetKey struct{}

type byteBudget struct {
	mu        sync.Mutex
	tenant    Tenant
	allowance int64
	remaining int64
}

func (b Budget) Context(ctx context.Context, tenant Tenant) context.Context {
	remaining := b.Bytes
	if b.Items != 1 || remaining <= 0 || remaining > ByteLimit || tenant.Organization == "" || tenant.Generation <= 0 {
		remaining = 0
	}
	return context.WithValue(ctx, budgetKey{}, &byteBudget{tenant: tenant, allowance: remaining, remaining: remaining})
}

func RequireBudget(ctx context.Context, organization string) error {
	budget, ok := ctx.Value(budgetKey{}).(*byteBudget)
	if !ok || budget.tenant.Organization != organization || budget.tenant.Generation <= 0 || budget.allowance <= 0 {
		return errors.New("maintenance requires its selected tenant byte budget")
	}
	return nil
}

func RequireTenant(ctx context.Context, tenant Tenant) error {
	if err := RequireBudget(ctx, tenant.Organization); err != nil {
		return err
	}
	budget, ok := ctx.Value(budgetKey{}).(*byteBudget)
	if !ok || budget.tenant != tenant {
		return errors.New("maintenance allocation generation mismatch")
	}
	return nil
}

func RequireAllowance(ctx context.Context, organization string, minimum int64) error {
	if err := RequireBudget(ctx, organization); err != nil {
		return err
	}
	budget, ok := ctx.Value(budgetKey{}).(*byteBudget)
	if !ok || minimum <= 0 || budget.allowance < minimum {
		return ErrByteBudget
	}
	return nil
}

func charge(ctx context.Context, organization string, bytes int64) error {
	budget, ok := ctx.Value(budgetKey{}).(*byteBudget)
	if !ok {
		return nil
	}
	if err := RequireBudget(ctx, organization); err != nil {
		return err
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if bytes <= 0 || bytes > budget.remaining {
		return ErrByteBudget
	}
	budget.remaining -= bytes
	return nil
}

type Details struct{ store attachment.DetailStore }

func LimitDetails(store attachment.DetailStore) *Details {
	if limited, ok := store.(*Details); ok {
		return limited
	}
	return &Details{store: store}
}

func (s *Details) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if s.store == nil {
		return nil, artifact.ErrStorage
	}
	if ref.CompressedSize <= 0 || ref.CompressedSize > attachment.MaxArchiveBytes || ref.UncompressedSize <= 0 || ref.UncompressedSize > attachment.MaxArchiveBytes {
		return nil, artifact.ErrIntegrity
	}
	if err := charge(ctx, organization, ref.CompressedSize+ref.UncompressedSize); err != nil {
		return nil, err
	}
	raw, err := s.store.ReadDetail(ctx, organization, ref)
	if err == nil && int64(len(raw)) != ref.UncompressedSize {
		return nil, artifact.ErrIntegrity
	}
	return raw, err
}

func (s *Details) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if s.store == nil {
		return attachment.ArchiveReference{}, artifact.ErrStorage
	}
	size := int64(len(raw))
	compressedLimit := size + size/100 + 1024
	if size <= 0 || size > attachment.MaxArchiveBytes {
		return attachment.ArchiveReference{}, artifact.ErrIntegrity
	}
	if err := charge(ctx, organization, size+compressedLimit); err != nil {
		return attachment.ArchiveReference{}, err
	}
	ref, err := s.store.StoreDetail(ctx, organization, raw)
	if err == nil && (ref.UncompressedSize != size || ref.CompressedSize <= 0 || ref.CompressedSize > compressedLimit) {
		return attachment.ArchiveReference{}, artifact.ErrIntegrity
	}
	return ref, err
}
