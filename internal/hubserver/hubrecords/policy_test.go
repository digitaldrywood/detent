package hubrecords

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/policy"
)

func TestProjectPolicyCache(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		count      int
		otherScope bool
		otherDB    bool
		mutate     bool
	}{
		{name: "repeated authored policy decodes once", count: 1},
		{name: "two policies in two projects stay distinct", count: 2, otherScope: true},
		{name: "same policy in two projects stays scoped", count: 1, otherScope: true},
		{name: "same policy in two databases stays scoped", count: 1, otherDB: true},
		{name: "returned maps and slices are isolated", count: 1, mutate: true},
		{name: "oldest entry evicted at fixed bound", count: ProjectPolicyCacheLimit + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{
				ConfigPath: "detent.yaml", HasConfig: true,
				Config:       []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\ngate:\n  run: true\n  required_status_checks: []\n"),
				WorkflowPath: "WORKFLOW.md", Workflow: []byte("Implement the assigned issue.\n"),
			})
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := config.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			decodes := 0
			decode := func(raw string) (*config.ResolvedNativePolicy, error) {
				decodes++
				return DecodeResolvedProjectPolicy(raw)
			}
			cache := &ProjectPolicyCache{Decode: decode}
			ctx := context.WithValue(t.Context(), ProjectPolicyCacheContextKey{}, cache)
			scope := "organization/project"
			read := func(ctx context.Context, scope string, descriptor policy.Descriptor) policy.Descriptor {
				t.Helper()
				raw, err := json.Marshal(descriptor)
				if err != nil {
					t.Fatal(err)
				}
				approval, err := DecodeProjectPolicy(ctx, scope, descriptor.ID, string(raw), policy.Approval{})
				if err != nil {
					t.Fatal(err)
				}
				return approval.Policy
			}
			first := read(ctx, scope, descriptor)
			applied, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, first)
			if err != nil {
				t.Fatal(err)
			}
			cached, err := ProjectPolicyWorkflow(ctx, scope, first)
			if err != nil || !reflect.DeepEqual(cached, applied) {
				t.Fatalf("cached authored workflow differs from application: %v", err)
			}
			if test.mutate {
				first.Authored.Files["detent.yaml"] = "changed"
				first.Configuration.Behavior[0] = '!'
				first.Workflow.States[0].Transitions = []string{"changed"}
				cached.Authored.Files["WORKFLOW.md"] = "changed"
				cached.DefinitionSources.Config[0] = '!'
				cached.Config.Tracker.ActiveStates[0] = "changed"
				cached.Config.Tracker.PriorityMap.Map["changed"] = 1
				*cached.Config.Worker.AllowLocalBinding = true
				cached.Config = cached.Config.WithWorkerDefaults(config.WorkerDefaults{AllowLocalBinding: new(false)})
				if !cached.Config.Worker.EffectiveAllowLocalBinding() {
					t.Fatal("copied workflow lost its project worker setting")
				}
			}
			for i := 1; i < test.count; i++ {
				if i == ProjectPolicyCacheLimit {
					read(ctx, scope, descriptor)
				}
				other := hubtest.PolicyDescriptor()
				other.SourceRevision = fmt.Sprintf("%040x", i)
				other = other.WithID()
				otherScope := scope
				if test.otherScope {
					otherScope = "organization/other-project"
				}
				if got := read(ctx, otherScope, other); !reflect.DeepEqual(got, other) {
					t.Fatal("policy identities mixed")
				}
			}
			if test.otherScope && test.count == 1 {
				read(ctx, "organization/other-project", descriptor)
			}
			if test.otherDB {
				other := &ProjectPolicyCache{Decode: decode}
				read(context.WithValue(t.Context(), ProjectPolicyCacheContextKey{}, other), scope, descriptor)
			}
			if test.count > ProjectPolicyCacheLimit {
				if len(cache.Entries) != ProjectPolicyCacheLimit {
					t.Fatalf("cache entries = %d", len(cache.Entries))
				}
				if _, found := cache.Entries[ProjectPolicyCacheKey{Scope: scope, ID: descriptor.ID}]; found {
					t.Fatal("oldest policy retained past bound")
				}
			}
			if got := read(ctx, scope, descriptor); !reflect.DeepEqual(got, descriptor) {
				t.Fatal("cached descriptor changed")
			}
			again, err := ProjectPolicyWorkflow(ctx, scope, descriptor)
			if err != nil || !reflect.DeepEqual(again, applied) {
				t.Fatalf("cached workflow changed after read: %v", err)
			}
			want := test.count
			if test.otherDB || test.otherScope && test.count == 1 || test.count > ProjectPolicyCacheLimit {
				want++
			}
			if decodes != want {
				t.Fatalf("policy decodes = %d, want %d", decodes, want)
			}
		})
	}
}

func TestProjectPolicyCacheConcurrentReads(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		otherKey string
		fail     bool
		panic    bool
		evict    bool
	}{
		{name: "same key shares one decode and independent results"},
		{name: "unrelated warm hit completes during decode", otherKey: "warm"},
		{name: "unrelated cold miss completes during decode", otherKey: "cold"},
		{name: "failed decode reaches waiters and later retries", fail: true},
		{name: "panicked decode releases waiters and later retries", panic: true},
		{name: "eviction during decode preserves cache entries", evict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				descriptor := hubtest.PolicyDescriptor()
				descriptor.Requirements.RequiredTags = []string{"example"}
				descriptor = descriptor.WithID()
				raw, err := json.Marshal(descriptor)
				if err != nil {
					t.Fatal(err)
				}
				other := hubtest.PolicyDescriptor()
				otherRaw, err := json.Marshal(other)
				if err != nil {
					t.Fatal(err)
				}
				started, release := make(chan struct{}), make(chan struct{})
				resume := sync.OnceFunc(func() { close(release) })
				var readers sync.WaitGroup
				cache := &ProjectPolicyCache{}
				t.Cleanup(func() {
					resume()
					synctest.Wait()
					if t.Failed() {
						cache.Mu.Lock()
						for key, pending := range cache.Inflight {
							select {
							case <-pending.Done:
							default:
								pending.Err = errors.New("test cleanup released pending decode")
								close(pending.Done)
							}
							delete(cache.Inflight, key)
						}
						cache.Mu.Unlock()
					}
					readers.Wait()
				})
				var decodes atomic.Int32
				decodeErr := errors.New("example decode failure")
				const panicValue = "example private policy input"
				cache.Decode = func(input string) (*config.ResolvedNativePolicy, error) {
					if input == string(raw) && decodes.Add(1) == 1 {
						close(started)
						<-release
						if test.panic {
							panic(panicValue)
						}
						if test.fail {
							return nil, decodeErr
						}
					}
					return DecodeResolvedProjectPolicy(input)
				}
				ctx := context.WithValue(t.Context(), ProjectPolicyCacheContextKey{}, cache)
				scope := "organization/project"
				if test.otherKey == "warm" || test.evict {
					if _, err := DecodeProjectPolicy(ctx, scope, other.ID, string(otherRaw), policy.Approval{}); err != nil {
						t.Fatal(err)
					}
				}
				type readResult struct {
					approval   policy.Approval
					err        error
					panicValue any
				}
				const count = 8
				results := make(chan readResult, count)
				read := func() {
					defer readers.Done()
					defer func() {
						if value := recover(); value != nil {
							results <- readResult{panicValue: value}
						}
					}()
					approval, err := DecodeProjectPolicy(ctx, scope, descriptor.ID, string(raw), policy.Approval{})
					results <- readResult{approval: approval, err: err}
				}
				readers.Add(1)
				go read()
				<-started
				if !cache.Mu.TryLock() {
					t.Fatal("shared cache mutex held during decode")
				}
				cache.Mu.Unlock()
				for range count - 1 {
					readers.Add(1)
					go read()
				}
				if test.otherKey != "" {
					otherResult := make(chan readResult, 1)
					readers.Add(1)
					go func() {
						defer readers.Done()
						approval, err := DecodeProjectPolicy(ctx, scope, other.ID, string(otherRaw), policy.Approval{})
						otherResult <- readResult{approval: approval, err: err}
					}()
					synctest.Wait()
					select {
					case result := <-otherResult:
						if result.err != nil || !reflect.DeepEqual(result.approval.Policy, other) {
							t.Fatalf("unrelated read = %+v, %v", result.approval, result.err)
						}
					default:
						t.Fatal("unrelated read blocked on paused decode")
					}
				} else {
					synctest.Wait()
				}
				if got := decodes.Load(); got != 1 {
					t.Fatalf("paused decodes = %d, want 1", got)
				}
				if test.evict {
					for i := range ProjectPolicyCacheLimit {
						if _, err := DecodeProjectPolicy(ctx, fmt.Sprintf("replacement/%d", i), other.ID, string(otherRaw), policy.Approval{}); err != nil {
							t.Fatal(err)
						}
					}
					if _, ok := cache.Entries[ProjectPolicyCacheKey{Scope: scope, ID: other.ID}]; ok {
						t.Fatal("warm entry survived eviction during decode")
					}
				}
				resume()
				synctest.Wait()
				values := make([]policy.Descriptor, 0, count)
				panics := 0
				for range count {
					var result readResult
					select {
					case result = <-results:
					default:
						t.Fatal("reader remained blocked after decode completed")
					}
					if result.panicValue != nil {
						if !test.panic || result.panicValue != panicValue {
							t.Fatalf("unexpected decoder panic: %v", result.panicValue)
						}
						panics++
					} else if test.panic {
						if result.err == nil || result.err.Error() != "decode project policy panicked" || !reflect.DeepEqual(result.approval, policy.Approval{}) {
							t.Fatalf("panic waiter = %+v, %v", result.approval, result.err)
						}
					} else if test.fail {
						if !errors.Is(result.err, decodeErr) {
							t.Fatalf("shared decode error = %v, want %v", result.err, decodeErr)
						}
					} else {
						if result.err != nil || !reflect.DeepEqual(result.approval.Policy, descriptor) {
							t.Fatalf("concurrent read = %+v, %v", result.approval, result.err)
						}
						values = append(values, result.approval.Policy)
					}
				}
				readers.Wait()
				if test.panic && panics != 1 {
					t.Fatalf("decoder panics = %d, want 1", panics)
				}
				if !test.fail && !test.panic {
					values[0].Requirements.RequiredTags[0] = "changed"
					for _, value := range values[1:] {
						if !reflect.DeepEqual(value, descriptor) {
							t.Fatal("concurrent results share mutable values")
						}
					}
				}
				if test.evict {
					if len(cache.Entries) != ProjectPolicyCacheLimit || len(cache.Inflight) != 0 {
						t.Fatal("completed decode violated cache bound or retained pending entry")
					}
					if _, ok := cache.Entries[ProjectPolicyCacheKey{Scope: scope, ID: other.ID}]; ok {
						t.Fatal("completed decode restored an evicted entry")
					}
					if _, ok := cache.Entries[ProjectPolicyCacheKey{Scope: "replacement/0", ID: other.ID}]; ok {
						t.Fatal("completed decode did not evict the oldest replacement")
					}
					for i := 1; i < ProjectPolicyCacheLimit; i++ {
						if _, ok := cache.Entries[ProjectPolicyCacheKey{Scope: fmt.Sprintf("replacement/%d", i), ID: other.ID}]; !ok {
							t.Fatal("completed decode evicted a newer replacement")
						}
					}
					seen := make(map[ProjectPolicyCacheKey]bool)
					for _, key := range cache.Order {
						if _, ok := cache.Entries[key]; !ok || seen[key] {
							t.Fatal("eviction left stale or duplicate cache ordering")
						}
						seen[key] = true
					}
					if len(seen) != len(cache.Entries) {
						t.Fatal("completed decode published an orphaned cache entry")
					}
				}
				for range 2 {
					approval, err := DecodeProjectPolicy(ctx, scope, descriptor.ID, string(raw), policy.Approval{})
					if err != nil || !reflect.DeepEqual(approval.Policy, descriptor) {
						t.Fatalf("later read = %+v, %v", approval, err)
					}
				}
				want := int32(1)
				if test.fail || test.panic {
					want++
				}
				if got := decodes.Load(); got != want {
					t.Fatalf("decodes = %d, want %d", got, want)
				}
			})
		})
	}
}
