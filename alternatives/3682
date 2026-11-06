package platformusers

import (
	"net/mail"
	"strings"
)

const Limit = 50

type Query struct {
	Text  string `json:"q"`
	Exact bool   `json:"-"`
	After string `json:"after,omitempty"`
}

func Parse(text string) Query {
	text = strings.ToLower(strings.TrimSpace(text))
	address, err := mail.ParseAddress(text)
	return Query{Text: text, Exact: err == nil && address != nil && address.Address == text}
}

type Organization struct {
	ID   string `json:"organization_id"`
	Name string `json:"organization_name"`
}

type Membership struct {
	Organization
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
}

type Invitation struct {
	Organization
	ExpiresAt string `json:"expires_at"`
}

type User struct {
	Email        string       `json:"email"`
	Subject      string       `json:"subject"`
	PlatformRole string       `json:"platform_role"`
	LastSignInAt string       `json:"last_sign_in_at"`
	Memberships  []Membership `json:"memberships"`
	Invitations  []Invitation `json:"invitations"`
}

type Counts struct {
	All            int `json:"all"`
	Staff          int `json:"staff"`
	NoOrganization int `json:"no_organization"`
	Invited        int `json:"invited"`
}

type Result struct {
	Total         int            `json:"total"`
	TotalUsers    int            `json:"total_users"`
	Page          int            `json:"page"`
	PageSize      int            `json:"page_size"`
	Counts        Counts         `json:"counts"`
	Organizations []Organization `json:"organizations"`
	Users         []User         `json:"users"`
	Unsearched    []Organization `json:"unsearched"`
}

type TenantMember struct {
	Email    string `json:"email"`
	Subject  string `json:"subject"`
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
}

type TenantInvitation struct {
	Email     string `json:"email"`
	ExpiresAt string `json:"expires_at"`
}

type TenantResult struct {
	NextCursor  string             `json:"next_cursor,omitempty"`
	Members     []TenantMember     `json:"members"`
	Invitations []TenantInvitation `json:"invitations"`
}
