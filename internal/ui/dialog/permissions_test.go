package dialog

import (
	"image"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func newTestPermissions(t *testing.T) *Permissions {
	t.Helper()
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s}
	perm := permission.PermissionRequest{
		ID:         "perm-test",
		ToolCallID: "tool-call-test",
		ToolName:   "bash",
	}
	return NewPermissions(com, perm)
}

// TestPermissionsMouseClickFiresButtonOption proves the cached button
// compositors let a click commit the option under the pointer: Allow on the
// left of the button row allows, Deny on the right denies.
func TestPermissionsMouseClickFiresButtonOption(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	scr := uv.NewScreenBuffer(100, 40)
	p.Draw(scr, image.Rect(0, 0, 100, 40))
	require.NotEmpty(t, p.buttonHits)
	require.False(t, p.buttonArea.Empty())

	// Sweep the strip to find each button's x, then click it.
	findX := func(col int) int {
		for x := p.buttonArea.Min.X; x < p.buttonArea.Max.X; x++ {
			if common.HitButtonIndex(p.buttonHits[0], x, p.buttonArea.Min.Y) == col {
				return x
			}
		}
		t.Fatalf("button column %d not hit-testable in the cached strip", col)
		return -1
	}

	action := p.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: findX(0), Y: p.buttonArea.Min.Y, Button: tea.MouseLeft}))
	resp, ok := action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, PermissionAllow, resp.Action)

	last := len(p.buttonOptIdx[0]) - 1
	action = p.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: findX(last), Y: p.buttonArea.Min.Y, Button: tea.MouseLeft}))
	resp, ok = action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, PermissionDeny, resp.Action)

	// Right clicks never resolve the request.
	action = p.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: findX(0), Y: p.buttonArea.Min.Y, Button: tea.MouseRight}))
	require.Nil(t, action)
}

// TestPermissions_ActionKeysResolve verifies that action keys produce the
// correct permission response.
func TestPermissions_ActionKeysResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key    tea.KeyPressMsg
		action PermissionAction
	}{
		{keyMsg('a'), PermissionAllow},
		{keyMsg('A'), PermissionAllow},
		{keyMsg('d'), PermissionDeny},
		{keyMsg('D'), PermissionDeny},
		{keyMsg('s'), PermissionAllowForSession},
		{keyMsg('S'), PermissionAllowForSession},
	}

	for _, tc := range tests {
		p := newTestPermissions(t)
		action := p.HandleMsg(tc.key)
		resp, ok := action.(ActionPermissionResponse)
		require.Truef(t, ok, "key %q should produce ActionPermissionResponse", tc.key.Text)
		require.Equal(t, tc.action, resp.Action)
	}
}

// TestPermissions_NavigationCyclesOptions verifies that tab and arrow keys
// cycle through the three permission options.
func TestPermissions_NavigationCyclesOptions(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	require.Equal(t, 0, p.selectedOption)

	// Tab cycles forward.
	p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, 1, p.selectedOption)

	p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, 2, p.selectedOption)

	// Wrap around.
	p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, 0, p.selectedOption)

	// Left cycles backward.
	p.HandleMsg(keyMsg('h'))
	require.Equal(t, 2, p.selectedOption)
}

// TestPermissions_EnterConfirmsSelection verifies that enter confirms the
// currently selected option.
func TestPermissions_EnterConfirmsSelection(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	p.selectedOption = 1 // Allow for session.

	action := p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	resp, ok := action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, PermissionAllowForSession, resp.Action)
}

// TestPermissions_EscapeDenies verifies that escape denies the request.
func TestPermissions_EscapeDenies(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	action := p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	resp, ok := action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, PermissionDeny, resp.Action)
}

// TestPermissions_ScopeTiers verifies the Cmd / Cmd+Args session tiers:
// keybinds, option cycling, and the namespaced subject stored with the
// chosen tier.
func TestPermissions_ScopeTiers(t *testing.T) {
	t.Parallel()

	scoped := func() *Permissions {
		p := newTestPermissions(t)
		p.permission.Subject = "git"
		p.permission.SubjectFull = "git commit"
		return p
	}

	// g grants the args tier verbatim, namespaced.
	p := scoped()
	action := p.HandleMsg(keyMsg('g'))
	resp, ok := action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, PermissionAllowForSession, resp.Action)
	require.Equal(t, "args:git commit", resp.Permission.Subject)

	// s grants the cmd tier.
	p = scoped()
	action = p.HandleMsg(keyMsg('s'))
	resp, ok = action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, "cmd:git", resp.Permission.Subject)

	// Cycling covers four options: deny is last, then wrap to zero.
	p = scoped()
	require.Equal(t, 4, p.numOptions())
	for range 2 {
		p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
		require.NotEqual(t, 3, p.selectedOption, "deny should be last")
	}
	p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, 3, p.selectedOption, "fourth option is deny")
	p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, 0, p.selectedOption)

	// Enter on option 2 is the args tier, option 3 denies.
	p.selectedOption = 2
	action = p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	resp, ok = action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, "args:git commit", resp.Permission.Subject)

	p = scoped()
	p.selectedOption = 3
	action = p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	resp, ok = action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, PermissionDeny, resp.Action)
}

// TestPermissions_NoScopeTiersKeepsThreeOptions verifies calls without two
// distinct scopes (the common non-bash case) keep the original three-option
// dialog and verbatim session subject.
func TestPermissions_NoScopeTiersKeepsThreeOptions(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	require.Equal(t, 3, p.numOptions())
	require.False(t, p.hasScopeTiers())

	p.selectedOption = 1
	action := p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	resp, ok := action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, PermissionAllowForSession, resp.Action)
	require.Empty(t, resp.Permission.Subject, "legacy grant stays verbatim")
}

// TestPermissions_UnknownScopeOffersNoSessionGrant verifies that a command
// whose binaries could not be determined offers only Allow and Deny, so a
// session grant can never be minted from the unknown marker and reused.
func TestPermissions_UnknownScopeOffersNoSessionGrant(t *testing.T) {
	t.Parallel()

	unknown := func() *Permissions {
		p := newTestPermissions(t)
		p.permission.Subject = permission.ScopeUnknown
		p.permission.SubjectFull = permission.ScopeUnknown
		return p
	}

	p := unknown()
	require.Equal(t, 2, p.numOptions(), "only Allow and Deny")
	require.False(t, p.canGrantSession())
	require.False(t, p.hasScopeTiers())

	// Cycling cannot land on a session button.
	for range 4 {
		p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
		require.Less(t, p.selectedOption, 2)
	}

	// The session key falls back to allowing this call only.
	p = unknown()
	action := p.HandleMsg(keyMsg('s'))
	resp, ok := action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, PermissionAllow, resp.Action, "session key must not create a grant")

	// Enter on the last option denies.
	p = unknown()
	p.selectedOption = 1
	action = p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	resp, ok = action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, PermissionDeny, resp.Action)

	// The rendered button row must not offer any session grant. Buttons are
	// rendered with styles that split their words, so strip before matching.
	buttons := ansi.Strip(p.renderButtons(120, false))
	require.NotContains(t, buttons, "Session")
	require.Contains(t, buttons, "Allow")
	require.Contains(t, buttons, "Deny")
}
