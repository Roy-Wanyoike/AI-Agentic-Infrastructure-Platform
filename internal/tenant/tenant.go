package tenant

import "strings"

type Context struct {
	OrganizationID string
	UserID         string
	Role           string
}

func NormalizeOrganizationID(value string) string {
	return strings.TrimSpace(value)
}

func NewContext(orgID, userID, role string) Context {
	return Context{
		OrganizationID: NormalizeOrganizationID(orgID),
		UserID:         strings.TrimSpace(userID),
		Role:           strings.TrimSpace(role),
	}
}

func (c Context) IsValid() bool {
	return c.OrganizationID != "" && c.UserID != "" && c.Role != ""
}
