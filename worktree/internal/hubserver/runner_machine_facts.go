package hubserver

type machineSpriteFacts struct {
	Name         *string `json:"sprite_name"`
	WokenAt      *string `json:"sprite_woken_at"`
	WakeFailedAt *string `json:"sprite_wake_failed_at"`
}

func spriteFactValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
