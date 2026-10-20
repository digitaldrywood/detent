package cloudentry

import (
	"cmp"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/platformusers"
)

func pagePlatformUsers(result platformusers.Result, filters url.Values) platformusers.Result {
	result.TotalUsers = len(result.Users)
	users := make([]platformusers.User, 0, len(result.Users))
	for _, user := range result.Users {
		if role := filters.Get("role"); role != "" && role != "all" && user.PlatformRole != role && (role != "none" || user.PlatformRole != "") {
			continue
		}
		if organization := filters.Get("organization"); organization != "" && organization != "all" {
			member := slices.ContainsFunc(user.Memberships, func(m platformusers.Membership) bool { return m.ID == organization })
			invited := slices.ContainsFunc(user.Invitations, func(i platformusers.Invitation) bool { return i.ID == organization })
			if !member && !invited {
				continue
			}
		}
		staff := user.PlatformRole != ""
		noOrganization := len(result.Unsearched) == 0 && len(user.Memberships) == 0
		invited := len(user.Invitations) > 0
		result.Counts.All++
		if staff {
			result.Counts.Staff++
		}
		if noOrganization {
			result.Counts.NoOrganization++
		}
		if invited {
			result.Counts.Invited++
		}
		if filters.Get("segment") == "staff" && !staff || filters.Get("segment") == "no_organization" && !noOrganization || filters.Get("segment") == "invited" && !invited {
			continue
		}
		users = append(users, user)
	}
	field := filters.Get("sort")
	descending := filters.Get("direction") == "desc" || filters.Get("direction") != "asc" && (field == "" || field == "last_sign_in")
	slices.SortFunc(users, func(a, b platformusers.User) int {
		var order int
		switch field {
		case "email":
			order = cmp.Compare(a.Email, b.Email)
		case "role":
			order = cmp.Compare(a.PlatformRole, b.PlatformRole)
		case "organizations":
			order = cmp.Compare(len(a.Memberships), len(b.Memberships))
		default:
			at, err := time.Parse(time.RFC3339Nano, a.LastSignInAt)
			if err != nil {
				at = time.Time{}
			}
			bt, err := time.Parse(time.RFC3339Nano, b.LastSignInAt)
			if err != nil {
				bt = time.Time{}
			}
			order = at.Compare(bt)
		}
		if descending {
			order = -order
		}
		if order == 0 {
			return cmp.Compare(a.Email, b.Email)
		}
		return order
	})
	result.Total = len(users)
	result.PageSize = platformusers.Limit
	page, err := strconv.Atoi(filters.Get("page"))
	if err != nil {
		page = 1
	}
	result.Page = min(max(page, 1), max(1, (result.Total+platformusers.Limit-1)/platformusers.Limit))
	start := (result.Page - 1) * platformusers.Limit
	result.Users = users[start:min(start+platformusers.Limit, result.Total)]
	return result
}
