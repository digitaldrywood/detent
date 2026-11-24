package attachmentsweep

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type Sweep struct {
	Storage attachment.Storage
	Now     func() time.Time
	Lookup  func(attachment.ObjectLookup) (int, []byte, error)
	Lock    func() func()
	Audit   func() error
}

func (s Sweep) expired(object attachment.Object) bool {
	return !object.CreatedAt.After(s.Now().Add(-attachment.OrphanTTL))
}

func (s Sweep) lookup(input attachment.ObjectLookup, result any) error {
	status, raw, err := s.Lookup(input)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return errors.New("attachment existence metadata unavailable")
	}
	return json.Unmarshal(raw, result)
}

func (s Sweep) exists(input attachment.ObjectLookup) (bool, error) {
	var result struct {
		Exists *bool `json:"exists"`
	}
	if err := s.lookup(input, &result); err != nil {
		return false, err
	}
	if result.Exists == nil {
		return false, errors.New("attachment existence metadata missing")
	}
	return *result.Exists, nil
}

func (s Sweep) remove(ctx context.Context, key string, input attachment.ObjectLookup) error {
	unlock := s.Lock()
	defer unlock()
	current, found, err := s.Storage.Stat(ctx, key)
	if err != nil || !found || !s.expired(current) {
		return err
	}
	exists, err := s.exists(input)
	if err != nil || exists {
		return err
	}
	if err := s.Audit(); err != nil {
		return err
	}
	return s.Storage.Delete(ctx, key)
}

func (s Sweep) Run(ctx context.Context, organization, namespace string) error {
	return s.run(ctx, organization, namespace, s.Storage.Walk)
}

func (s Sweep) Page(ctx context.Context, organization, namespace string, objects []attachment.Object) error {
	return s.run(ctx, organization, namespace, func(ctx context.Context, prefix string, visit func(attachment.Object) error) error {
		for _, obj := range objects {
			if !strings.HasPrefix(obj.Key, prefix) {
				return attachment.ErrInvalid
			}
			if err := visit(obj); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
}

type walkObjects func(context.Context, string, func(attachment.Object) error) error

func (s Sweep) run(ctx context.Context, organization, namespace string, walk walkObjects) error {
	prefix, err := attachment.OrganizationPrefix(organization)
	if err != nil {
		return err
	}
	prefix += namespace
	if namespace == "blobs/sha256/" {
		return s.archives(ctx, prefix, walk)
	}
	return walk(ctx, prefix, func(object attachment.Object) error {
		if !s.expired(object) {
			return nil
		}
		input := attachment.ObjectLookup{Key: object.Key}
		if namespace == "attachments/" {
			id := strings.TrimPrefix(object.Key, prefix)
			key, err := attachment.Key(organization, id)
			if err != nil || key != object.Key {
				return attachment.ErrInvalid
			}
			input = attachment.ObjectLookup{ID: id}
		}
		exists, err := s.exists(input)
		if err != nil || exists {
			return err
		}
		return s.remove(ctx, object.Key, input)
	})
}

func (s Sweep) archives(ctx context.Context, prefix string, walk walkObjects) error {
	keys := make([]string, 0, attachment.ReferenceBatchSize)
	flush := func() error {
		if len(keys) == 0 {
			return nil
		}
		var referenced []*bool
		if err := s.lookup(attachment.ObjectLookup{Keys: keys}, &referenced); err != nil {
			return err
		}
		if len(referenced) != len(keys) {
			return errors.New("incomplete archive reference response")
		}
		for i, exists := range referenced {
			if exists == nil {
				return errors.New("missing archive reference result")
			}
			if !*exists {
				if err := s.remove(ctx, keys[i], attachment.ObjectLookup{Key: keys[i]}); err != nil {
					return err
				}
			}
		}
		keys = keys[:0]
		return nil
	}
	err := walk(ctx, prefix, func(object attachment.Object) error {
		if s.expired(object) {
			keys = append(keys, object.Key)
		}
		if len(keys) == cap(keys) {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}
