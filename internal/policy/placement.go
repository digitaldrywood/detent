package policy

import "errors"

type Placement struct {
	Mode           string `json:"mode"`
	TodoThreshold  int    `json:"todo_threshold,omitempty"`
	OverflowSlots  int    `json:"overflow_slots,omitempty"`
	MaxSpriteSlots int    `json:"max_sprite_slots,omitempty"`
}

func (p Placement) Resolved() Placement {
	if p.Mode == "" {
		p.Mode = "blended"
	}
	return p
}

func (p Placement) Validate() error {
	p = p.Resolved()
	if p.MaxSpriteSlots < 0 || p.MaxSpriteSlots > 100 {
		return errors.New("maximum Sprite slots must be between 0 and 100; zero preserves existing runner capacity limits")
	}
	switch p.Mode {
	case "blended", "sprites_only":
		if p.TodoThreshold != 0 || p.OverflowSlots != 0 {
			return errors.New("todo threshold and overflow slots apply only to local_first placement")
		}
	case "local_first":
		if p.TodoThreshold < 1 || p.OverflowSlots < 1 || p.OverflowSlots > 100 {
			return errors.New("local_first placement requires an explicit positive Todo threshold and 1 to 100 overflow slots")
		}
	default:
		return errors.New("placement mode must be blended, sprites_only or local_first")
	}
	return nil
}

func (p Placement) SpriteLimit() int {
	p = p.Resolved()
	if p.Mode == "local_first" {
		if p.MaxSpriteSlots == 0 {
			return p.OverflowSlots
		}
		return min(p.MaxSpriteSlots, p.OverflowSlots)
	}
	return p.MaxSpriteSlots
}
