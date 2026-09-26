package policy

import (
	"errors"
	"strings"
	"testing"

	"agent-gateway/internal/protocol"
)

// principalFor builds a principal the way Authenticate would: canonical scope,
// nil for an admin.
func principalFor(role Role, nodeIDs ...string) Principal {
	if role == Admin {
		return Principal{ID: "prn_test", Role: role}
	}
	return Principal{ID: "prn_test", Role: role, NodeIDs: nodeIDs}
}

// TestAuthorizeMatrix is the permission matrix itself, written out as a table
// of expectations rather than derived from the implementation. Each row says
// whether a role may perform an action at all, whether that action is scoped
// to a node, and the test evaluates each row three ways: on a node the
// principal holds, on a node it does not, and with no node named at all.
func TestAuthorizeMatrix(t *testing.T) {
	// Every role/action pair, evaluated against a scope that contains testNode.
	cases := []struct {
		role    Role
		action  string
		allowed bool // the role may perform the action within its scope
		scoped  bool // the action addresses one node and needs a node id
	}{
		// Admin is global: every action, and the management actions do not
		// need a node at all.
		{role: Admin, action: actionTaskRead, allowed: true, scoped: true},
		{role: Admin, action: actionTaskSubmit, allowed: true, scoped: true},
		{role: Admin, action: actionTaskCancel, allowed: true, scoped: true},
		{role: Admin, action: actionDeviceRevoke, allowed: true, scoped: false},
		{role: Admin, action: actionCredentialManage, allowed: true, scoped: false},

		// Operator: the three task actions inside its scope, nothing else.
		{role: Operator, action: actionTaskRead, allowed: true, scoped: true},
		{role: Operator, action: actionTaskSubmit, allowed: true, scoped: true},
		{role: Operator, action: actionTaskCancel, allowed: true, scoped: true},
		{role: Operator, action: actionDeviceRevoke, allowed: false, scoped: false},
		{role: Operator, action: actionCredentialManage, allowed: false, scoped: false},

		// Viewer: read only, inside its scope.
		{role: Viewer, action: actionTaskRead, allowed: true, scoped: true},
		{role: Viewer, action: actionTaskSubmit, allowed: false, scoped: true},
		{role: Viewer, action: actionTaskCancel, allowed: false, scoped: true},
		{role: Viewer, action: actionDeviceRevoke, allowed: false, scoped: false},
		{role: Viewer, action: actionCredentialManage, allowed: false, scoped: false},
	}

	for _, tc := range cases {
		name := string(tc.role) + "/" + tc.action
		t.Run(name, func(t *testing.T) {
			p := principalFor(tc.role, testNode)

			// A node the principal holds.
			wantErr := Authorize(p, tc.action, testNode)
			if tc.allowed && wantErr != nil {
				t.Errorf("Authorize(%s, %s, %s) = %v, want allowed", tc.role, tc.action, testNode, wantErr)
			}
			if !tc.allowed && !errors.Is(wantErr, protocol.ErrUnauthorized) {
				t.Errorf("Authorize(%s, %s, %s) = %v, want protocol.ErrUnauthorized", tc.role, tc.action, testNode, wantErr)
			}

			// No node named. A scoped action must refuse, whatever the role:
			// task.read aimed at nothing is not a read of everything.
			noNodeErr := Authorize(p, tc.action, "")
			mayUseNoNode := tc.allowed && !tc.scoped
			if mayUseNoNode && noNodeErr != nil {
				t.Errorf("Authorize(%s, %s) with no node = %v, want allowed", tc.role, tc.action, noNodeErr)
			}
			if !mayUseNoNode && !errors.Is(noNodeErr, protocol.ErrUnauthorized) {
				t.Errorf("Authorize(%s, %s) with no node = %v, want protocol.ErrUnauthorized", tc.role, tc.action, noNodeErr)
			}

			// A node the principal does not hold. Only the global role may
			// reach it; for a management action the node is ignored, so a
			// non-admin is refused for the role reason instead.
			otherErr := Authorize(p, tc.action, otherNode)
			if tc.role == Admin && tc.allowed {
				if otherErr != nil {
					t.Errorf("authorize(admin, %s, %s) = %v, want allowed: an admin is global", tc.action, otherNode, otherErr)
				}
				return
			}
			if !errors.Is(otherErr, protocol.ErrUnauthorized) {
				t.Errorf("Authorize(%s, %s, %s) = %v, want protocol.ErrUnauthorized", tc.role, tc.action, otherNode, otherErr)
			}
		})
	}
}

// TestAuthorizeRefusesUnknownRolesAndActions covers the "anything else is
// denied" half of the contract: a role or action the vocabulary does not
// define must not fall through to a permissive default.
func TestAuthorizeRefusesUnknownRolesAndActions(t *testing.T) {
	known := Principal{ID: "prn_test", Role: Viewer, NodeIDs: []string{testNode}}

	unknownActions := []string{"", "task.delete", "TASK.READ", "task.read ", " task.read", "task.read.submit", "device.revoke.all", "admin"}
	for _, action := range unknownActions {
		err := Authorize(known, action, testNode)
		if !errors.Is(err, protocol.ErrUnauthorized) {
			t.Errorf("Authorize(viewer, %q) = %v, want protocol.ErrUnauthorized", action, err)
		}
		// An unknown action must be refused even for the most privileged
		// role: nothing about "admin" makes an undefined action defined.
		if err := Authorize(Principal{ID: "prn_admin", Role: Admin}, action, testNode); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Errorf("Authorize(admin, %q) = %v, want protocol.ErrUnauthorized", action, err)
		}
	}

	unknownRoles := []Role{"", "superuser", "ADMIN", "Admin", "owner", "root"}
	for _, role := range unknownRoles {
		for _, action := range []string{actionTaskRead, actionTaskSubmit, actionTaskCancel, actionDeviceRevoke, actionCredentialManage} {
			p := Principal{ID: "prn_test", Role: role, NodeIDs: []string{testNode}}
			if err := Authorize(p, action, testNode); !errors.Is(err, protocol.ErrUnauthorized) {
				t.Errorf("Authorize(role %q, %s) = %v, want protocol.ErrUnauthorized", role, action, err)
			}
		}
	}
}

// TestAuthorizeRefusesAScopeThatContradictsItsRole covers the malformed
// principal cases: an "admin" carrying a scope, and a scoped role carrying
// none. Neither may be repaired into authority — an admin with a scope must not
// become global, and an empty scope must not become every node.
func TestAuthorizeRefusesAScopeThatContradictsItsRole(t *testing.T) {
	adminWithScope := Principal{ID: "prn_test", Role: Admin, NodeIDs: []string{testNode}}
	for _, action := range []string{actionTaskRead, actionTaskSubmit, actionTaskCancel} {
		if err := Authorize(adminWithScope, action, testNode); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Errorf("Authorize(admin with scope, %s) = %v, want protocol.ErrUnauthorized", action, err)
		}
	}

	for _, role := range []Role{Operator, Viewer} {
		empty := Principal{ID: "prn_test", Role: role}
		for _, action := range []string{actionTaskRead, actionTaskSubmit, actionTaskCancel} {
			if err := Authorize(empty, action, testNode); !errors.Is(err, protocol.ErrUnauthorized) {
				t.Errorf("Authorize(%s with empty scope, %s) = %v, want protocol.ErrUnauthorized", role, action, err)
			}
		}
	}
}

// TestAuthorizeIsExactAndCaseSensitive pins down that scope matching is string
// equality: no wildcard, no prefix, no case folding, no trimming.
func TestAuthorizeIsExactAndCaseSensitive(t *testing.T) {
	p := Principal{ID: "prn_test", Role: Operator, NodeIDs: []string{"node-1", "node-12"}}
	allowed := []string{"node-1", "node-12"}
	for _, nodeID := range allowed {
		if err := Authorize(p, actionTaskRead, nodeID); err != nil {
			t.Errorf("Authorize(operator, task.read, %q) = %v, want allowed", nodeID, err)
		}
	}
	denied := []string{
		"node",     // prefix of a scope entry
		"node_1",   // separator
		"Node-1",   // case
		"NODE-1",   // case
		"node-1 ",  // trailing space
		" node-1",  // leading space
		"node-1x",  // longer
		"node-*",   // wildcard
		"*",        // wildcard
		"node-13",  // neighbouring id
		"node-123", // another neighbouring id
	}
	for _, nodeID := range denied {
		if err := Authorize(p, actionTaskRead, nodeID); !errors.Is(err, protocol.ErrUnauthorized) {
			t.Errorf("Authorize(operator, task.read, %q) = %v, want protocol.ErrUnauthorized", nodeID, err)
		}
	}
}

// TestAuthorizeDenialMessagesCarryNoRequestValues checks that a refusal cannot
// be used to reflect caller-supplied bytes into a log line, and that every
// refusal is the same sentinel.
func TestAuthorizeDenialMessagesCarryNoRequestValues(t *testing.T) {
	const action = "task.delete"
	const nodeID = "node-secret-name"
	p := Principal{ID: "prn_test", Role: Operator, NodeIDs: []string{testNode}}

	for _, err := range []error{
		Authorize(p, action, nodeID),
		Authorize(p, actionTaskSubmit, nodeID),
		Authorize(Principal{ID: "prn_test", Role: "superuser"}, actionTaskRead, nodeID),
		Authorize(Principal{ID: "prn_test", Role: Viewer}, actionTaskRead, testNode),
	} {
		if !errors.Is(err, protocol.ErrUnauthorized) {
			t.Fatalf("a refusal = %v, want protocol.ErrUnauthorized", err)
		}
		if strings.Contains(err.Error(), action) || strings.Contains(err.Error(), nodeID) {
			t.Fatalf("refusal reflects request values: %v", err)
		}
	}
}

// TestAuthorizeAllowsTheScopeToBeAHundredNodes is the size boundary: the limit
// bounds what a caller may grant, and everything inside it still matches.
func TestAuthorizeAllowsTheScopeToBeAHundredNodes(t *testing.T) {
	scope := make([]string, 0, maxScopeNodes)
	for i := 0; i < maxScopeNodes; i++ {
		scope = append(scope, nodeName(i))
	}
	p := Principal{ID: "prn_test", Role: Viewer, NodeIDs: scope}
	for _, nodeID := range scope {
		if err := Authorize(p, actionTaskRead, nodeID); err != nil {
			t.Fatalf("Authorize(viewer, task.read, %q) = %v, want allowed", nodeID, err)
		}
	}
	if err := Authorize(p, actionTaskRead, nodeName(maxScopeNodes)); !errors.Is(err, protocol.ErrUnauthorized) {
		t.Fatalf("Authorize with a node just past the scope = %v, want protocol.ErrUnauthorized", err)
	}
}

// nodeName returns a distinct node id for index i.
func nodeName(i int) string {
	return "node-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
}
