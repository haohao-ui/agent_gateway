package policy

import "context"

// Role is the permission level of an operator principal. The values are the
// persisted form (they are what the role column contains), so they must not be
// renamed without a migration.
type Role string

const (
	// Admin may perform every action on every node. An admin is global: its
	// scope is empty by construction, and an empty scope means "all nodes"
	// only for this role.
	Admin Role = "admin"

	// Operator may read, submit and cancel tasks, but only within the nodes it
	// was issued for. It may not revoke devices or manage credentials.
	Operator Role = "operator"

	// Viewer may read tasks within its nodes and do nothing else.
	Viewer Role = "viewer"
)

// Principal is an operator identity as returned by Authenticate.
//
// ID is server-generated and Role is one of the three constants above;
// NodeIDs is the canonical scope: sorted, deduplicated, and empty only for an
// admin. The struct carries no credential — a token is never stored in it, so
// logging or serialising a Principal cannot leak one.
type Principal struct {
	ID      string
	Role    Role
	NodeIDs []string
}

const (
	// Action constants for authorization decisions.
	ActionTaskRead         = "task.read"
	ActionTaskSubmit       = "task.submit"
	ActionTaskCancel       = "task.cancel"
	ActionDeviceRevoke     = "device.revoke"
	ActionCredentialManage = "credential.manage"

	actionTaskRead         = ActionTaskRead
	actionTaskSubmit       = ActionTaskSubmit
	actionTaskCancel       = ActionTaskCancel
	actionDeviceRevoke     = ActionDeviceRevoke
	actionCredentialManage = ActionCredentialManage
)

type principalContextKey struct{}

var principalCtxKey = principalContextKey{}

// WithPrincipal stores the Principal in the context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey, p)
}

// PrincipalFromContext extracts the Principal from the context if present.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	if ctx == nil {
		return Principal{}, false
	}
	p, ok := ctx.Value(principalCtxKey).(Principal)
	return p, ok
}

// nodeScopedActions are the actions that address one node and therefore
// require a non-empty node ID and a scope that covers it. The remaining two
// are management actions: they act on the gateway's own state, they are
// admin-only, and the node ID is ignored for them.
var nodeScopedActions = map[string]bool{
	actionTaskRead:   true,
	actionTaskSubmit: true,
	actionTaskCancel: true,
}

// permissions is the whole authorization matrix, written out rather than
// derived. A role that is not a key here has no permissions at all, which is
// what makes an unrecognised role safe by default instead of accidentally
// privileged.
var permissions = map[Role]map[string]bool{
	Admin: {
		actionTaskRead:         true,
		actionTaskSubmit:       true,
		actionTaskCancel:       true,
		actionDeviceRevoke:     true,
		actionCredentialManage: true,
	},
	Operator: {
		actionTaskRead:   true,
		actionTaskSubmit: true,
		actionTaskCancel: true,
	},
	Viewer: {
		actionTaskRead: true,
	},
}

// Authorize decides whether p may perform action on nodeID and returns nil if
// it may. Every refusal wraps protocol.ErrUnauthorized.
//
// It is a pure decision: no database, no clock, no context. It cannot consult
// revocation, because it has no store to consult — Authenticate is what
// establishes that a credential was live at the moment it was presented, and
// the two are meant to be used together, with Authorize called on the
// Principal Authenticate returned. A caller that assembles a Principal from
// request data has bypassed authentication, not authorization.
//
// The rules it enforces:
//
//   - The role must be one of the three known roles and the action one of the
//     five known actions; anything else is refused.
//   - The scope must match the role: an admin holds no node IDs (it is
//     global), an operator or viewer holds at least one. A value that
//     disagrees with its own role — an "admin" that carries a scope, a viewer
//     with an empty one — is refused rather than repaired, because repairing it
//     would hand out global authority from a malformed value.
//   - A node-scoped action requires a non-empty node ID and, for a
//     non-admin, an exact match in the scope. Matching is string equality:
//     there is no wildcard, no prefix rule and no case folding, so a scope can
//     never cover more than the nodes it spells out.
//   - A management action ignores the node ID entirely, which is why an empty
//     node ID is accepted for device.revoke and credential.manage.
//
// Denials carry no values from the request: an action or node ID that failed
// is not echoed back into a log line.
func Authorize(p Principal, action, nodeID string) error {
	granted, knownRole := permissions[p.Role]
	if !knownRole {
		return unauthorizedf("principal role is not a known role")
	}
	if !knownAction(action) {
		return unauthorizedf("action is not a known action")
	}
	if !granted[action] {
		return unauthorizedf("role is not permitted to perform this action")
	}
	if err := checkScopeShape(p); err != nil {
		return err
	}
	if !nodeScopedActions[action] {
		return nil
	}
	if nodeID == "" {
		return unauthorizedf("node-scoped action requires a node id")
	}
	if p.Role == Admin {
		return nil
	}
	if !containsNode(p.NodeIDs, nodeID) {
		return unauthorizedf("node is outside the principal scope")
	}
	return nil
}

// knownAction reports whether action is part of the frozen vocabulary.
func knownAction(action string) bool {
	_, ok := nodeScopedActions[action]
	if ok {
		return true
	}
	switch action {
	case actionDeviceRevoke, actionCredentialManage:
		return true
	}
	return false
}

// checkScopeShape enforces the invariant that ties a role to its scope.
func checkScopeShape(p Principal) error {
	if p.Role == Admin {
		if len(p.NodeIDs) > 0 {
			return unauthorizedf("principal scope contradicts its role")
		}
		return nil
	}
	if len(p.NodeIDs) == 0 {
		return unauthorizedf("principal scope contradicts its role")
	}
	return nil
}

// containsNode reports whether nodeID is in the scope. The list is short
// (maxScopeNodes) and already sorted, so a linear scan is the whole story.
func containsNode(scope []string, nodeID string) bool {
	for _, n := range scope {
		if n == nodeID {
			return true
		}
	}
	return false
}
