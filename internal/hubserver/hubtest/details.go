package hubtest

import (
	"bytes"
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type Details struct {
	mu   sync.Mutex
	data map[string][]byte
}

func NewDetails() *Details {
	return &Details{data: map[string][]byte{}}
}

func (d *Details) StoreDetail(_ context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data[ref.Key] = bytes.Clone(data)
	return ref, nil
}

func (d *Details) ReadDetail(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	data, ok := d.data[ref.Key]
	if !ok {
		return nil, artifact.ErrMissing
	}
	return bytes.Clone(data), nil
}

func (d *Details) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.data)
}

var _ attachment.DetailStore = (*Details)(nil)
