package workspace

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"io"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const changeSourceObjectOverhead = 1024

func changeSourcePackBudget(ctx context.Context, pack []byte) error {
	invalid := errors.New("source bundle exceeds the verification budget or has an invalid pack; split large changes or recreate it with git bundle create <path> HEAD ^<base>")
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return invalid
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	count := uint64(binary.BigEndian.Uint32(pack[8:12]))
	if version != 2 && version != 3 || count > tracker.MaxChangeDiffBytes/changeSourceObjectOverhead {
		return invalid
	}
	remaining := uint64(tracker.MaxChangeDiffBytes) - count*changeSourceObjectOverhead
	reader := bytes.NewReader(pack[12 : len(pack)-20])
	buffer := make([]byte, 32<<10)
	for range count {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.ReadByte()
		if err != nil {
			return invalid
		}
		kind, size := (header>>4)&7, uint64(header&15)
		for shift := uint(4); header&128 != 0; shift += 7 {
			if shift > 60 {
				return invalid
			}
			header, err = reader.ReadByte()
			if err != nil || uint64(header&127) > remaining>>shift {
				return invalid
			}
			size |= uint64(header&127) << shift
		}
		if size > remaining {
			return invalid
		}
		remaining -= size
		switch kind {
		case 1, 2, 3, 4:
		case 6:
			for n := 0; ; n++ {
				value, err := reader.ReadByte()
				if err != nil || n >= 9 {
					return invalid
				}
				if value&128 == 0 {
					break
				}
			}
		case 7:
			if _, err := io.CopyN(io.Discard, reader, 20); err != nil {
				return invalid
			}
		default:
			return invalid
		}
		inflated, err := zlib.NewReader(reader)
		if err != nil {
			return invalid
		}
		var prefix [20]byte
		prefixBytes := 0
		var total uint64
		for {
			if err := ctx.Err(); err != nil {
				return errors.Join(err, inflated.Close())
			}
			n, readErr := inflated.Read(buffer[:min(uint64(len(buffer)), size-total+1)])
			prefixBytes += copy(prefix[prefixBytes:], buffer[:n])
			total += uint64(n)
			if total > size || readErr != nil {
				closeErr := inflated.Close()
				if total != size || !errors.Is(readErr, io.EOF) || closeErr != nil {
					return invalid
				}
				break
			}
		}
		if kind == 6 || kind == 7 {
			base, n := binary.Uvarint(prefix[:prefixBytes])
			if n <= 0 || base > tracker.MaxChangeDiffBytes {
				return invalid
			}
			result, n := binary.Uvarint(prefix[n:prefixBytes])
			if n <= 0 || result > remaining {
				return invalid
			}
			remaining -= result
		}
	}
	if reader.Len() != 0 {
		return invalid
	}
	return nil
}
