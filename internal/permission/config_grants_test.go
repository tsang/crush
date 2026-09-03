package permission

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCutScopedEntry(t *testing.T) {
	t.Parallel()

	tool, subject, ok := CutScopedEntry("bash:cmd:mkdir,touch")
	require.True(t, ok)
	assert.Equal(t, "bash", tool)
	assert.Equal(t, "cmd:mkdir,touch", subject)

	tool, subject, ok = CutScopedEntry("bash:args:swift build")
	require.True(t, ok)
	assert.Equal(t, "bash", tool)
	assert.Equal(t, "args:swift build", subject)

	for _, entry := range []string{"bash", "bash:execute", "bash:cmd:", "bash:args:", "", "cmd:x"} {
		_, _, ok := CutScopedEntry(entry)
		assert.Falsef(t, ok, "%q is not a scoped entry", entry)
	}
}

func TestConfigScopedGrantCovers(t *testing.T) {
	t.Parallel()

	svc := NewPermissionService("/tmp", false, []string{
		"bash:cmd:mkdir,swift,touch",
		"bash:args:view diff",
	}).(*permissionService)

	covered := []PermissionRequest{
		{ToolName: "bash", Action: "execute", Path: "/tmp", Subject: "mkdir"},
		{ToolName: "bash", Action: "execute", Path: "/tmp", Subject: "mkdir,swift"},
		{ToolName: "bash", Action: "execute", Path: "/tmp", Subject: "view", SubjectFull: "view diff"},
	}
	for _, req := range covered {
		assert.Truef(t, svc.grantCovers(req), "config grant should cover %q", req.Subject)
	}

	notCovered := []PermissionRequest{
		{ToolName: "bash", Action: "execute", Path: "/tmp", Subject: "rm"},
		{ToolName: "bash", Action: "execute", Path: "/tmp", Subject: "mkdir,rm"},
		{ToolName: "bash", Action: "execute", Path: "/tmp", Subject: "view", SubjectFull: "view push"},
		{ToolName: "bash", Action: "execute", Path: "/tmp", Subject: ScopeUnknown},
		{ToolName: "edit", Action: "write", Path: "/tmp/x", Subject: "mkdir"},
	}
	for _, req := range notCovered {
		assert.Falsef(t, svc.grantCovers(req), "%q must still prompt", req.Subject)
	}
}

// TestAllowedToolsSourceLiveUpdate proves config-scoped grants take effect
// the moment the config changes, with no restart: the source closure is the
// live view, re-read on every lookup.
func TestAllowedToolsSourceLiveUpdate(t *testing.T) {
	t.Parallel()

	entries := []string{"bash:cmd:mkdir"}
	svc := NewPermissionService("/tmp", false, nil, WithAllowedToolsSource(func() []string {
		return entries
	})).(*permissionService)

	req := PermissionRequest{ToolName: "bash", Action: "execute", Path: "/tmp", Subject: "touch"}
	require.False(t, svc.grantCovers(req), "touch is not granted yet")

	entries = append(slices.Clone(entries), "bash:cmd:touch")
	require.True(t, svc.grantCovers(req), "persisted entry must apply without restart")

	entries = nil
	require.False(t, svc.grantCovers(req), "revoked entry must stop covering")
}

// TestSessionGrantsStillIsolate guards the invariant that scoped config
// grants do not widen session behavior: an unknown subject never rides a
// config grant, and the session tier keeps working alongside.
func TestSessionGrantsStillIsolate(t *testing.T) {
	t.Parallel()

	svc := NewPermissionService("/tmp", false, []string{"bash:cmd:mkdir"}).(*permissionService)

	// A session args grant stays exact and separate from the config cmd tier.
	sessionKey := PermissionKey{SessionID: "s1", ToolName: "bash", Action: "execute", Path: "/tmp"}
	svc.sessionPermissions.Set(sessionKey.WithSubject(ScopeSubject(ScopeArgs, "swift build")), true)

	assert.True(t, svc.grantCovers(PermissionRequest{
		SessionID: "s1", ToolName: "bash", Action: "execute", Path: "/tmp",
		Subject: "swift", SubjectFull: "swift build",
	}))
	assert.False(t, svc.grantCovers(PermissionRequest{
		SessionID: "s1", ToolName: "bash", Action: "execute", Path: "/tmp",
		Subject: "swift", SubjectFull: "swift push",
	}), "args tier must stay exact even with config grants loaded")
}
