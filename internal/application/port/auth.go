package port

import "context"

type Principal struct {
	Subject     string
	ProviderID  string
	Permissions map[string]struct{}
}

func (p Principal) HasPermission(permission string) bool {
	_, ok := p.Permissions[permission]
	return ok
}

type TokenVerifier interface {
	Verify(context.Context, string) (Principal, error)
}
