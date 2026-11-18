package reviewoutput

import (
	"fmt"
	"unicode/utf8"
)

const SourcePageBytes = 16 * 1024

type Range struct {
	Offset int64
	Length int
}

type Capture struct {
	Selection Range
	Total     int64
	Page      []byte
}

func (c *Capture) Write(data []byte) (int, error) {
	count := len(data)
	captureStart := max(c.Selection.Offset-(utf8.UTFMax-1), 0)
	start := min(max(captureStart-c.Total, 0), int64(count))
	length := min(c.Selection.Length+2*(utf8.UTFMax-1)-len(c.Page), count-int(start))
	if length > 0 {
		if c.Page == nil {
			c.Page = make([]byte, 0, c.Selection.Length+2*(utf8.UTFMax-1))
		}
		c.Page = append(c.Page, data[start:start+int64(length)]...)
	}
	c.Total += int64(count)
	return count, nil
}

func (c *Capture) Result() (string, bool) {
	start := min(c.Selection.Offset, c.Total)
	var content []byte
	if start < c.Total {
		captureStart := max(c.Selection.Offset-(utf8.UTFMax-1), 0)
		first := int(start - captureStart)
		lead := first
		for lead > 0 && !utf8.RuneStart(c.Page[lead]) {
			lead--
		}
		_, width := utf8.DecodeRune(c.Page[lead:])
		if width > 1 && lead+width > first {
			first = lead
		}
		end := min(first+c.Selection.Length, len(c.Page))
		if end < len(c.Page) && !utf8.RuneStart(c.Page[end]) {
			lead = end
			for lead > first && !utf8.RuneStart(c.Page[lead]) {
				lead--
			}
			_, width = utf8.DecodeRune(c.Page[lead:])
			if width > 1 && lead+width > end {
				end = lead
				if end == first {
					end = lead + width
				}
			}
		}
		start = captureStart + int64(first)
		content = c.Page[first:end]
	}
	end := start + int64(len(content))
	output := fmt.Sprintf("[source bytes %d:%d of %d; next_offset=%d; eof=%t]\n", start, end, c.Total, end, end == c.Total) + string(content)
	return output, start > 0 || end < c.Total
}
