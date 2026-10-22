// Package primitives holds the redesign design-system building blocks.
// Every component uses only the @theme tokens from static/css/input.css:
// five semantic colors (ok/warn/err/info + neutral) plus one teal accent
// reserved for interactivity. Status is never conveyed by color alone —
// components pair color with a shape glyph and text.
package primitives

import "strconv"

// Kind names a semantic status color.
type Kind string

const (
	KindOK      Kind = "ok"
	KindWarn    Kind = "warn"
	KindErr     Kind = "err"
	KindInfo    Kind = "info"
	KindNeutral Kind = "neutral"
)

// Glyph returns the shape paired with each status so state is readable
// without color: hexagon=error, triangle=warning, check=success, dot=neutral.
func (k Kind) Glyph() string {
	switch k {
	case KindErr:
		return "⬣"
	case KindWarn:
		return "▲"
	case KindOK:
		return "✓"
	default:
		return "●"
	}
}

func (k Kind) dotClass() string {
	switch k {
	case KindOK:
		return "bg-ok"
	case KindWarn:
		return "bg-warn"
	case KindErr:
		return "bg-err"
	case KindInfo:
		return "bg-info"
	default:
		return "bg-dim"
	}
}

func (k Kind) textClass() string {
	switch k {
	case KindOK:
		return "text-ok"
	case KindWarn:
		return "text-warn"
	case KindErr:
		return "text-err"
	case KindInfo:
		return "text-info"
	default:
		return "text-sec"
	}
}

func moreCountLabel(count int) string {
	return strconv.Itoa(count)
}

// chipClass returns the passive tint styling for a chip: 15% alpha
// background of the semantic color plus colored text.
func (k Kind) chipClass() string {
	switch k {
	case KindOK:
		return "bg-ok/15 text-ok"
	case KindWarn:
		return "bg-warn/15 text-warn"
	case KindErr:
		return "bg-err/15 text-err"
	case KindInfo:
		return "bg-info/15 text-info"
	default:
		return "bg-elev text-sec"
	}
}
