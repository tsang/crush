package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func newTestGrantsReview(t *testing.T) *GrantsReview {
	t.Helper()
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s}
	return NewGrantsReview(com, []string{
		"bash",
		"bash:cmd:mkdir,touch",
		"bash:args:swift build",
	})
}

func TestGrantsReviewFiltersUnscoped(t *testing.T) {
	t.Parallel()
	g := newTestGrantsReview(t)
	require.Len(t, g.rows, 2, "unscoped entries are config-level, not reviewed here")
	require.True(t, g.HasRows())
	require.Equal(t, "bash cmd  mkdir, touch", g.rows[0].label)
	require.Equal(t, "bash args  swift build", g.rows[1].label)
	for _, row := range g.rows {
		require.True(t, row.keep, "everything starts checked")
	}
}

func TestGrantsReviewToggleAndApply(t *testing.T) {
	t.Parallel()
	g := newTestGrantsReview(t)

	g.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	g.HandleMsg(keyMsg(' '))
	action := g.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	saved, ok := action.(ActionSaveGrants)
	require.True(t, ok)
	require.Equal(t, []string{"bash:cmd:mkdir,touch"}, saved.Kept)
}

func TestGrantsReviewDigitToggles(t *testing.T) {
	t.Parallel()
	g := newTestGrantsReview(t)
	g.HandleMsg(keyMsg('2'))
	action := g.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	saved, ok := action.(ActionSaveGrants)
	require.True(t, ok)
	require.Equal(t, []string{"bash:cmd:mkdir,touch"}, saved.Kept)
}

func TestGrantsReviewCloseKeepsAll(t *testing.T) {
	t.Parallel()
	g := newTestGrantsReview(t)
	g.HandleMsg(keyMsg('1')) // uncheck then recheck nothing: toggle off
	g.HandleMsg(keyMsg('1')) // back on
	action := g.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	saved, ok := action.(ActionSaveGrants)
	require.True(t, ok)
	require.Equal(t, []string{"bash:cmd:mkdir,touch", "bash:args:swift build"}, saved.Kept,
		"closing without deliberate changes must keep every grant")
}

func TestGrantsReviewEmptyCloses(t *testing.T) {
	t.Parallel()
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s}
	g := NewGrantsReview(com, []string{"bash", "view"})
	require.False(t, g.HasRows())
	action := g.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, ok := action.(ActionClose)
	require.True(t, ok)
}
