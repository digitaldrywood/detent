package schemafingerprint

import "testing"

func TestVersionsDistinguishesMigrationSetsWithSameMaximum(t *testing.T) {
	first := Versions([]int64{1, 3, 5})
	second := Versions([]int64{1, 4, 5})
	if first == second {
		t.Fatalf("different migration sets have the same fingerprint: %q", first)
	}
	if first != Versions([]int64{1, 3, 5}) {
		t.Fatalf("fingerprint is not stable: %q", first)
	}
}
