package hubrecords

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/policy"
)

var ErrNoApprovedRepositoryPolicy = &hubapi.NativeError{Code: "policy_not_approved", Message: "No approved repository policy; resolve the customer definition and ask an administrator to approve its descriptor", Status: http.StatusConflict}

const ProjectPolicyCacheLimit = 256

type ProjectPolicyCacheKey struct {
	Scope string `json:"-"`
	ID    string `json:"-"`
}

type ProjectPolicyCache struct {
	Mu       sync.Mutex                                                     `json:"-"`
	Entries  map[ProjectPolicyCacheKey]*workflowconfig.ResolvedNativePolicy `json:"-"`
	Inflight map[ProjectPolicyCacheKey]*ProjectPolicyDecode                 `json:"-"`
	Order    []ProjectPolicyCacheKey                                        `json:"-"`
	oldest   int
	Decode   func(string) (*workflowconfig.ResolvedNativePolicy, error) `json:"-"`
}

type ProjectPolicyDecode struct {
	Done  chan struct{} `json:"-"`
	value *workflowconfig.ResolvedNativePolicy
	Err   error `json:"-"`
}

type ProjectPolicyCacheContextKey struct{}

func ProjectPolicyCacheFromContext(ctx context.Context) *ProjectPolicyCache {
	cache, ok := ctx.Value(ProjectPolicyCacheContextKey{}).(*ProjectPolicyCache)
	if !ok {
		return nil
	}
	return cache
}

func DecodeResolvedProjectPolicy(raw string) (*workflowconfig.ResolvedNativePolicy, error) {
	var descriptor policy.Descriptor
	if err := json.Unmarshal([]byte(raw), &descriptor); err != nil {
		return nil, err
	}
	return workflowconfig.ResolveNativePolicy(descriptor)
}

func (c *ProjectPolicyCache) Resolve(scope, id, raw string) (*workflowconfig.ResolvedNativePolicy, error) {
	if c == nil {
		return DecodeResolvedProjectPolicy(raw)
	}
	c.Mu.Lock()
	key := ProjectPolicyCacheKey{Scope: scope, ID: id}
	if value, ok := c.Entries[key]; ok {
		c.Mu.Unlock()
		return value, nil
	}
	if pending, ok := c.Inflight[key]; ok {
		c.Mu.Unlock()
		<-pending.Done
		return pending.value, pending.Err
	}
	pending := &ProjectPolicyDecode{Done: make(chan struct{})}
	if c.Inflight == nil {
		c.Inflight = make(map[ProjectPolicyCacheKey]*ProjectPolicyDecode)
	}
	c.Inflight[key] = pending
	decode := c.Decode
	c.Mu.Unlock()
	if decode == nil {
		decode = DecodeResolvedProjectPolicy
	}
	value, err := func() (*workflowconfig.ResolvedNativePolicy, error) {
		defer func() {
			if value := recover(); value != nil {
				c.Mu.Lock()
				pending.Err = errors.New("decode project policy panicked")
				delete(c.Inflight, key)
				close(pending.Done)
				c.Mu.Unlock()
				panic(value)
			}
		}()
		return decode(raw)
	}()
	c.Mu.Lock()
	defer c.Mu.Unlock()
	pending.value, pending.Err = value, err
	delete(c.Inflight, key)
	close(pending.Done)
	if err != nil {
		return nil, err
	}
	if c.Entries == nil {
		c.Entries = make(map[ProjectPolicyCacheKey]*workflowconfig.ResolvedNativePolicy)
	}
	if len(c.Order) == ProjectPolicyCacheLimit {
		delete(c.Entries, c.Order[c.oldest])
		c.Order[c.oldest] = key
		c.oldest = (c.oldest + 1) % ProjectPolicyCacheLimit
	} else {
		c.Order = append(c.Order, key)
	}
	c.Entries[key] = value
	return value, nil
}

func ProjectPolicyWorkflow(ctx context.Context, scope string, descriptor policy.Descriptor) (workflowconfig.Workflow, error) {
	cache := ProjectPolicyCacheFromContext(ctx)
	if cache != nil {
		cache.Mu.Lock()
		resolved := cache.Entries[ProjectPolicyCacheKey{Scope: scope, ID: descriptor.ID}]
		cache.Mu.Unlock()
		if resolved != nil {
			return resolved.Workflow()
		}
	}
	return workflowconfig.ApplyNativePolicy(workflowconfig.Workflow{Config: workflowconfig.Default()}, descriptor)
}
