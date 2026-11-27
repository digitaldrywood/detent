package executionapi

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func TestParseRunnerSessionIDs(t *testing.T) {
	t.Parallel()

	maxID := strconv.FormatInt(math.MaxInt64, 10)
	maxBatch := strings.Repeat(maxID+",", 199) + maxID
	overBatch := strings.Repeat("1,", 200) + "1"

	tests := []struct {
		name    string
		raw     string
		want    []int64
		wantErr error
	}{
		{name: "empty", raw: ""},
		{name: "valid ids", raw: "1,42,999", want: []int64{1, 42, 999}},
		{name: "200 max int64 ids accepted", raw: maxBatch, want: slices.Repeat([]int64{math.MaxInt64}, 200)},
		{name: "201 ids rejected", raw: overBatch, wantErr: corestore.ErrInvalidPage},
		{name: "oversized comma payload rejected before splitting", raw: strings.Repeat(",", 1<<20), wantErr: corestore.ErrInvalidPage},
		{name: "oversized whitespace payload rejected before trimming", raw: strings.Repeat(" ", 4000) + "1", wantErr: corestore.ErrInvalidPage},
		{name: "non-positive rejected", raw: "0", wantErr: corestore.ErrInvalidPage},
		{name: "non-numeric rejected", raw: "abc", wantErr: corestore.ErrInvalidPage},
		{name: "trailing comma rejected", raw: "1,", wantErr: corestore.ErrInvalidPage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseRunnerSessionIDs(tt.raw, 200)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("parseRunnerSessionIDs() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && !slices.Equal(got, tt.want) {
				t.Fatalf("parseRunnerSessionIDs() = %v, want %v", got, tt.want)
			}
		})
	}
}
