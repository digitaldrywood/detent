package tracker

import "time"

type ReleasePickInput struct {
	SourceIssueID  NativeWorkItemID `json:"source_issue_id"`
	SourceChangeID string           `json:"source_change_id"`
	SourceMergeSHA string           `json:"source_merge_sha"`
	Repository     string           `json:"repository"`
	PRNumber       int              `json:"pr_number"`
	HeadRef        string           `json:"head_ref"`
	HeadSHA        string           `json:"head_sha"`
	BaseRef        string           `json:"base_ref"`
	BaseSHA        string           `json:"base_sha"`
	FrozenSHA      string           `json:"frozen_sha"`
}

type AssociateReleasePick struct {
	Mutation
	ReleasePickInput
	ExpectedRevision Revision `json:"expected_revision,string"`
}

type ReleaseDelivery struct {
	ReleasePickInput
	ChangeID     string    `json:"change_id"`
	VersionID    string    `json:"version_id"`
	Actor        Actor     `json:"actor"`
	AssociatedAt time.Time `json:"associated_at"`
}
