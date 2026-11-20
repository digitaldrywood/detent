package tracker

type NativePhaseReceipt struct {
	Mutation
	Phase       string `json:"phase"`
	AttemptID   string `json:"attempt_id"`
	Branch      string `json:"branch"`
	HeadSHA     string `json:"head_sha"`
	TreeSHA     string `json:"tree_sha"`
	PolicyID    string `json:"policy_id"`
	InputDigest string `json:"input_digest"`
}

type NativePhaseContinuation struct {
	Workflow      []CollaborationEvent `json:"workflow,omitempty"`
	RepositoryURL string               `json:"repository_url,omitempty"`
	LastPushed    *NativePhaseReceipt  `json:"last_pushed,omitempty"`
	NextPhase     string               `json:"next_phase,omitempty"`
	Receipts      []NativePhaseReceipt `json:"receipts"`
}
