package hubcontent

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

const ClaimEvidenceLimit = 1000

type ClaimHydration struct {
	Run func(context.Context, hubapi.NativeQueryer) error `json:"-"`
}

func (*ClaimHydration) Error() string { return "claim content requires preparation" }

type ExplanationReadKey struct{}

func ExplanationRead(ctx context.Context) bool {
	value, ok := ctx.Value(ExplanationReadKey{}).(bool)
	return ok && value
}

type NativeRuntimePreparationKey struct{}

type NativeRuntimeObjectKey struct {
	Kind, Organization string                      `json:"-"`
	Reference          attachment.ArchiveReference `json:"-"`
	Presign            bool                        `json:"-"`
}

type NativeRuntimePreparation struct {
	collecting bool
	objects    map[NativeRuntimeObjectKey]NativeRuntimeObjectResult
}

type NativeRuntimeObjectResult struct {
	value any
	err   error
}

type NativeRuntimeHydration struct {
	read func()
}

func (*NativeRuntimeHydration) Error() string { return "runtime object requires hydration" }

func ReadRuntimeObject[T any](ctx context.Context, key NativeRuntimeObjectKey, read func() (T, error)) (T, error) {
	prepared, ok := ctx.Value(NativeRuntimePreparationKey{}).(*NativeRuntimePreparation)
	if !ok || !prepared.collecting {
		return read()
	}
	if value, found := prepared.objects[key]; found {
		decoded, ok := value.value.(T)
		if !ok {
			var zero T
			return zero, errors.New("runtime object cache type mismatch")
		}
		return decoded, value.err
	}
	var zero T
	return zero, &NativeRuntimeHydration{read: func() {
		value, err := read()
		prepared.objects[key] = NativeRuntimeObjectResult{value: value, err: err}
	}}
}
