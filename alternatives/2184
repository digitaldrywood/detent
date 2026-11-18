package hubtest

import (
	"fmt"
	"testing"
)

func ContractPathSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, path := range paths {
		set[path] = true
	}
	return set
}

func ContractJoinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// CompareContractShape asserts that the Go payload carries exactly the
// fixture's key set at every level and agrees on whether a value is null.
// Values themselves are not compared: the fixtures are examples, the shape
// is the contract.
func CompareContractShape(t *testing.T, path string, want, got any, extra, optional map[string]bool) {
	t.Helper()
	label := path
	if label == "" {
		label = "(root)"
	}
	if (want == nil) != (got == nil) {
		t.Errorf("%s: fixture null = %t, Go null = %t", label, want == nil, got == nil)
		return
	}
	if want == nil {
		return
	}
	switch wanted := want.(type) {
	case map[string]any:
		gotten, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: fixture is an object, Go value is %T", label, got)
			return
		}
		for key, value := range wanted {
			child := ContractJoinPath(path, key)
			other, present := gotten[key]
			if !present {
				if !optional[child] {
					t.Errorf("%s: Go value is missing key %q", label, key)
				}
				continue
			}
			CompareContractShape(t, child, value, other, extra, optional)
		}
		if extra[path] {
			return
		}
		for key := range gotten {
			if _, present := wanted[key]; present || extra[ContractJoinPath(path, key)] {
				continue
			}
			t.Errorf("%s: Go value has extra key %q", label, key)
		}
	case []any:
		gotten, ok := got.([]any)
		if !ok {
			t.Errorf("%s: fixture is an array, Go value is %T", label, got)
			return
		}
		if len(wanted) != len(gotten) {
			t.Errorf("%s: fixture has %d elements, Go value has %d", label, len(wanted), len(gotten))
			return
		}
		for i := range wanted {
			CompareContractShape(t, path+"[]", wanted[i], gotten[i], extra, optional)
		}
	default:
		if fmt.Sprintf("%T", want) != fmt.Sprintf("%T", got) {
			t.Errorf("%s: fixture is %T, Go value is %T", label, want, got)
		}
	}
}
