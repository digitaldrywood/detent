package hubserver

import (
	"strings"
)

func analyticsDimension(values ...string) string {
	var key strings.Builder
	key.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			key.WriteByte(',')
		}
		key.WriteByte('"')
		for j := range len(value) {
			c := value[j]
			switch c {
			case '"', '\\':
				key.WriteByte('\\')
				key.WriteByte(c)
			case '\b':
				key.WriteString(`\b`)
			case '\t':
				key.WriteString(`\t`)
			case '\n':
				key.WriteString(`\n`)
			case '\f':
				key.WriteString(`\f`)
			case '\r':
				key.WriteString(`\r`)
			default:
				if c < 0x20 {
					key.WriteString(`\u00`)
					key.WriteByte("0123456789abcdef"[c>>4])
					key.WriteByte("0123456789abcdef"[c&0xf])
				} else {
					key.WriteByte(c)
				}
			}
		}
		key.WriteByte('"')
	}
	key.WriteByte(']')
	return key.String()
}
