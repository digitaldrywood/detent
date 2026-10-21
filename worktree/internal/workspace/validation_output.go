package workspace

import (
	"fmt"
	"unicode/utf8"
)

type reviewOutputCapture struct {
	selection ReviewOutputRange
	total     int64
	page      []byte
}

func (c *reviewOutputCapture) Write(data []byte) (int, error) {
	count := len(data)
	captureStart := max(c.selection.Offset-(utf8.UTFMax-1), 0)
	start := min(max(captureStart-c.total, 0), int64(count))
	length := min(c.selection.Length+2*(utf8.UTFMax-1)-len(c.page), count-int(start))
	if length > 0 {
		if c.page == nil {
			c.page = make([]byte, 0, c.selection.Length+2*(utf8.UTFMax-1))
		}
		c.page = append(c.page, data[start:start+int64(length)]...)
	}
	c.total += int64(count)
	return count, nil
}

func (c *reviewOutputCapture) result() (string, bool) {
	start := min(c.selection.Offset, c.total)
	var content []byte
	if start < c.total {
		captureStart := max(c.selection.Offset-(utf8.UTFMax-1), 0)
		first := int(start - captureStart)
		lead := first
		for lead > 0 && !utf8.RuneStart(c.page[lead]) {
			lead--
		}
		_, width := utf8.DecodeRune(c.page[lead:])
		if width > 1 && lead+width > first {
			first = lead
		}
		end := min(first+c.selection.Length, len(c.page))
		if end < len(c.page) && !utf8.RuneStart(c.page[end]) {
			lead = end
			for lead > first && !utf8.RuneStart(c.page[lead]) {
				lead--
			}
			_, width = utf8.DecodeRune(c.page[lead:])
			if width > 1 && lead+width > end {
				end = lead
				if end == first {
					end = lead + width
				}
			}
		}
		start = captureStart + int64(first)
		content = c.page[first:end]
	}
	end := start + int64(len(content))
	output := fmt.Sprintf("[source bytes %d:%d of %d; next_offset=%d; eof=%t]\n", start, end, c.total, end, end == c.total) + string(content)
	return output, start > 0 || end < c.total
}
