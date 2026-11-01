package accessstore

import (
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func RoleAllows(role string, scope apikey.Scope, action ...string) bool {
	if !auth.ValidOrganizationRole(role) {
		return false
	}
	if role == "reporter" && len(action) == 1 && operatortool.IsAdministration(action[0]) {
		return false
	}
	if scope == apikey.ScopeRead {
		return true
	}
	if scope == apikey.ScopeWrite {
		if role == "reporter" {
			if len(action) != 1 {
				return false
			}
			switch action[0] {
			case operatortool.EditItem, operatortool.FileIssue, operatortool.UploadAttachment, operatortool.ReferenceAttachment, operatortool.AddComment, operatortool.EditComment, operatortool.MoveItem, operatortool.VerifyFix, "create_thread", "post_to_thread", "resolve_thread", "credentials":
				return true
			}
			return false
		}
		return role != "viewer"
	}
	return scope == apikey.ScopeAdmin && (role == "owner" || role == "admin")
}

func LesserRole(a, b string) string {
	rank := map[string]int{"viewer": 0, "reporter": 1, "member": 2, "admin": 3, "owner": 4}
	if !auth.ValidOrganizationRole(a) || !auth.ValidOrganizationRole(b) {
		return ""
	}
	if rank[a] <= rank[b] {
		return a
	}
	return b
}
