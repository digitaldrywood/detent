package conversations

type QueryMessageData struct {
	Tool                 *string `json:"tool"`
	ItemID               *string `json:"item_id"`
	Status               *string `json:"status"`
	OperatorActionResult *string `json:"operator_action_result"`
	OperatorAction       *struct {
		Action *struct {
			ID    *string `json:"id"`
			Agent *string `json:"agent"`
		} `json:"action"`
	} `json:"operator_action"`
}
