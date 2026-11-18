package attachmentsweep

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type Sweep struct {
	Storage attachment.Storage
	Now     func() time.Time
	Lookup  func(attachment.ObjectLookup) (int, []byte, error)
	Lock    func(key string) (unlock func(), audit func() error, err error)
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

func (s Sweep) remove(ctx context.Context, key string, input attachment.ObjectLookup) (*time.Time, error) {
	unlock, audit, err := s.Lock(key)
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, found, err := s.Storage.Stat(ctx, key)
	if err != nil || !found {
		return nil, err
	}
	if !s.expired(current) {
		due := current.CreatedAt.Add(attachment.OrphanTTL)
		return &due, nil
	}
	exists, err := s.exists(input)
	if err != nil || exists {
		return nil, err
	}
	if err := audit(); err != nil {
		return nil, err
	}
	return nil, s.Storage.Delete(ctx, key)
}

func (s Sweep) Page(ctx context.Context, organization, namespace, after string) (string, error) {
	objects, next, err := s.Storage.ObjectPage(ctx, organization, namespace, after)
	if err != nil {
		return "", err
	}
	for _, object := range objects {
		if !s.expired(object) {
			continue
		}
		input := attachment.ObjectLookup{Key: object.Key}
		if namespace == "attachments/" {
			id := strings.TrimPrefix(object.Key, "orgs/"+organization+"/attachments/")
			key, err := attachment.Key(organization, id)
			if err != nil || key != object.Key {
				return "", attachment.ErrInvalid
			}
			input = attachment.ObjectLookup{ID: id}
		}
		exists, err := s.exists(input)
		if err != nil {
			return "", err
		}
		if !exists {
			if _, err := s.remove(ctx, object.Key, input); err != nil {
				return "", err
			}
		}
	}
	return next, nil
}

func (s Sweep) Archives(ctx context.Context, organization string, inventory bool) (bool, error) {
	var page *attachment.ReclaimPage
	if err := s.lookup(attachment.ObjectLookup{Page: true}, &page); err != nil {
		return false, err
	}
	if page == nil || page.Changed == nil || len(page.Changed) > attachment.ReferenceBatchSize {
		return false, errors.New("invalid archive reconciliation page")
	}
	var objects []attachment.Object
	next := page.After
	if inventory {
		var err error
		objects, next, err = s.Storage.ObjectPage(ctx, organization, "blobs/sha256/", page.After)
		if err != nil {
			return false, err
		}
	}
	receipt := attachment.ReclaimReceipt{After: page.After, Next: next, Checked: page.Changed}
	if !receipt.Valid(organization) {
		return false, attachment.ErrInvalid
	}
	changed := make(map[string]int, len(page.Changed))
	keys := make([]string, 0, len(page.Changed)+len(objects))
	for i, candidate := range page.Changed {
		changed[candidate.Key] = i
		keys = append(keys, candidate.Key)
	}
	for _, object := range objects {
		if _, already := changed[object.Key]; !already && s.expired(object) {
			keys = append(keys, object.Key)
		}
	}
	for batch := range slices.Chunk(keys, attachment.ReferenceBatchSize) {
		var referenced []*bool
		if err := s.lookup(attachment.ObjectLookup{Keys: batch}, &referenced); err != nil {
			return false, err
		}
		if len(referenced) != len(batch) || slices.Contains(referenced, nil) {
			return false, errors.New("incomplete archive reference response")
		}
		for i, exists := range referenced {
			if *exists {
				continue
			}
			key := batch[i]
			due, err := s.remove(ctx, key, attachment.ObjectLookup{Key: key})
			if err != nil {
				return false, err
			}
			if position, ok := changed[key]; ok {
				receipt.Checked[position].Due = due
			}
		}
	}
	if next == page.After && len(page.Changed) == 0 {
		return false, nil
	}
	var completed bool
	if err := s.lookup(attachment.ObjectLookup{Receipt: &receipt}, &completed); err != nil {
		return false, err
	}
	if !completed {
		return false, errors.New("archive reconciliation receipt refused")
	}
	return len(page.Changed) == attachment.ReferenceBatchSize, nil
}
