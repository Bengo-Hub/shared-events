package events

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// UserRelevance decides whether an auth.user.* event concerns this service, so services only
// provision the people who work in them. One auth-api tenant can run several products
// (codevertex-demo hosts every vertical), and role names such as manager, cashier and member are
// shared by all of them, so neither the tenant nor a generic role proves relevance.
//
// auth-api decides it now (2026-11-03): auth.user created/updated/pin_set events carry `services`,
// the services that should keep a local copy of the member ("*" = every service). With Service set,
// that list decides when present (Relevant), and Revoked tells a service to remove its copy. The
// rules below only apply to events without the list (older events, or auth-api could not decide).
//
// The rules, in order:
//  1. Admin-level roles (AdminRoles, defaulting to DefaultAdminRoles) are always relevant: a
//     tenant's administrators run every product it subscribes to.
//  2. When the event names outlets (outlet_ids or outlet_id), they decide: relevant when at least
//     one is an outlet this service mirrors (OutletKnown). Each service mirrors only the outlets
//     of its own use cases, so this is the precise signal.
//  3. Without outlets, a role unique to this service (ServiceRoles) is relevant.
//  4. Anything else is not. A person skipped here is still provisioned on their first real sign-in
//     to the service (JIT), so nobody who uses it is locked out.
type UserRelevance struct {
	// Service is this service's tag (pos, inventory, ...): the event's services list decides when
	// present. Empty keeps only the rules below.
	Service string
	// AdminRoles overrides DefaultAdminRoles when set.
	AdminRoles map[string]bool
	// ServiceRoles are role names only this service uses (doctor, waiter, librarian...).
	ServiceRoles map[string]bool
	// OutletKnown reports whether the service mirrors this outlet. Errors count as unknown.
	OutletKnown func(ctx context.Context, tenantID, outletID uuid.UUID) bool
}

// DefaultAdminRoles are the tenant-wide administrator roles auth-api issues.
var DefaultAdminRoles = map[string]bool{
	"superuser": true, "super_admin": true, "admin": true, "owner": true, "tenant_admin": true,
	"administrator": true, "director": true,
}

// Relevant applies the rules to one event payload.
func (r UserRelevance) Relevant(ctx context.Context, tenantID uuid.UUID, payload map[string]any) bool {
	if r.Service != "" {
		if services, known := PayloadServices(payload); known {
			return servicesAllow(services, r.Service)
		}
	}
	roles := PayloadStrings(payload["roles"])
	admin := r.AdminRoles
	if admin == nil {
		admin = DefaultAdminRoles
	}
	for _, role := range roles {
		if admin[strings.ToLower(role)] {
			return true
		}
	}
	if outlets := PayloadOutletIDs(payload); len(outlets) > 0 {
		if r.OutletKnown == nil {
			return false
		}
		for _, id := range outlets {
			if r.OutletKnown(ctx, tenantID, id) {
				return true
			}
		}
		return false
	}
	for _, role := range roles {
		if r.ServiceRoles[strings.ToLower(role)] {
			return true
		}
	}
	return false
}

// Revoked reports that the event's services list exists and leaves this service out: the member
// must not have a local copy here, so an existing one is removed. False without Service or without
// the list, so an older event or an undecided one never removes anybody.
func (r UserRelevance) Revoked(payload map[string]any) bool {
	if r.Service == "" {
		return false
	}
	services, known := PayloadServices(payload)
	return known && !servicesAllow(services, r.Service)
}

// AllServices is the services list entry meaning every service.
const AllServices = "*"

// PayloadServices reads the services list auth-api puts on auth.user events. known=false when
// the payload has none (null counts as none); an empty list is a decided "no services".
func PayloadServices(payload map[string]any) (services []string, known bool) {
	switch v := payload["services"].(type) {
	case []string:
		return v, true
	case []any:
		return PayloadStrings(v), true
	default:
		return nil, false
	}
}

func servicesAllow(services []string, service string) bool {
	for _, s := range services {
		if s == AllServices || s == service {
			return true
		}
	}
	return false
}

// PayloadStrings reads a JSON string list ([]any or []string) or a single string.
func PayloadStrings(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		if strings.TrimSpace(l) != "" {
			return []string{strings.TrimSpace(l)}
		}
	}
	return nil
}

// PayloadOutletIDs reads outlet_ids and outlet_id from an auth.user.* payload, deduplicated.
func PayloadOutletIDs(payload map[string]any) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, s := range append(PayloadStrings(payload["outlet_ids"]), PayloadStrings(payload["outlet_id"])...) {
		if id, err := uuid.Parse(s); err == nil && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
