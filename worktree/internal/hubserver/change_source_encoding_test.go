package hubserver

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func changeSourceBinaryFixture() []byte {
	random := rand.New(rand.NewPCG(1, 2))
	data := make([]byte, 128<<10)
	for offset := 0; offset < len(data); offset += 8 {
		binary.LittleEndian.PutUint64(data[offset:], random.Uint64())
	}
	return data
}

func TestChangeSourceEncodingRoundTrip(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		content []byte
	}{
		{"small", []byte("reviewed source\n")},
		{"text", bytes.Repeat([]byte("retained source\n"), 100000)},
		{"empty delta", nil},
		{"binary", changeSourceBinaryFixture()},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := changeTestInput()
			bundle := changeTestSource(t, &input, test.content)
			stored, err := encodeChangeSource(bundle)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("bundle=%d stored=%d ratio=%.4f encoding=%d", len(bundle), len(stored), float64(len(stored))/float64(len(bundle)), stored[5])
			if !bytes.HasPrefix(stored, []byte(changeSourceMagic+"\x01")) {
				t.Fatal("new source lacks its versioned frame")
			}
			if stored[5] == 1 && len(stored) >= len(bundle) {
				t.Fatal("compression enlarged the bundle")
			}
			if test.name == "binary" && stored[5] != 0 {
				t.Fatal("binary fixture did not exercise stored encoding")
			}
			for _, representation := range [][]byte{bundle, stored} {
				restored, err := decodeChangeSource(representation, input.Source.Bytes)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(restored, bundle) {
					t.Fatal("source round trip changed bytes")
				}
				if err := input.Source.Validate(input.BaseSHA, input.HeadSHA, restored); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestChangeSourceEncodingRefusesCorruptionAndExpansion(t *testing.T) {
	t.Parallel()
	const secret = "private-bundle-content"
	compressed := func(data []byte) []byte {
		t.Helper()
		var frame bytes.Buffer
		frame.WriteString(changeSourceMagic + "\x01\x01")
		writer := gzip.NewWriter(&frame)
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return frame.Bytes()
	}
	valid := compressed([]byte(secret))
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-1] ^= 255
	for _, test := range []struct {
		name   string
		stored []byte
		size   int64
	}{
		{"truncated frame", []byte(changeSourceMagic), 1},
		{"unknown version", []byte(changeSourceMagic + "\x02\x00" + secret), int64(len(secret))},
		{"unknown encoding", []byte(changeSourceMagic + "\x01\x02" + secret), int64(len(secret))},
		{"invalid gzip", []byte(changeSourceMagic + "\x01\x01" + secret), int64(len(secret))},
		{"truncated gzip", valid[:len(valid)-1], int64(len(secret))},
		{"checksum", corrupt, int64(len(secret))},
		{"trailing data", append(bytes.Clone(valid), secret...), int64(len(secret))},
		{"concatenated streams", append(bytes.Clone(valid), valid[6:]...), int64(len(secret))},
		{"recorded bound", valid, int64(len(secret) - 1)},
		{"short output", valid, int64(len(secret) + 1)},
		{"bundle bound", compressed(bytes.Repeat([]byte("x"), tracker.MaxChangeSourceBytes+1)), tracker.MaxChangeSourceBytes},
		{"invalid recorded bound", valid, tracker.MaxChangeSourceBytes + 1},
		{"invalid recorded size", valid, 0},
		{"stored bound", []byte(changeSourceMagic + "\x01\x00" + secret), 1},
		{"legacy bound", []byte(secret), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeChangeSource(test.stored, test.size); err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("expected content-free refusal, got %v", err)
			}
		})
	}
}
