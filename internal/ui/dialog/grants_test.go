package dialog

import (
	"image"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/ui/common"
	"github.com/charmbracelet/crush/internal/ui/styles"
	uv "github.com/charmbracelet/ultraviolet"
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
	require.Len(t, g.rows, 3, "unscoped entries are config-level and joined cmd entries flatten to one row per binary")
	require.True(t, g.HasRows())
	require.Equal(t, "bash cmd  mkdir", g.rows[0].label)
	require.Equal(t, "bash:cmd:mkdir", g.rows[0].raw)
	require.Equal(t, "bash cmd  touch", g.rows[1].label)
	require.Equal(t, "bash:cmd:touch", g.rows[1].raw)
	require.Equal(t, "bash args  swift build", g.rows[2].label)
	require.Equal(t, "bash:args:swift build", g.rows[2].raw)
	for _, row := range g.rows {
		require.True(t, row.keep, "everything starts checked")
	}
}

func TestGrantsReviewDedupesFlattenedRows(t *testing.T) {
	t.Parallel()
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s}
	g := NewGrantsReview(com, []string{
		"bash:cmd:mkdir,touch",
		"bash:cmd:touch",
	})
	require.Len(t, g.rows, 2, "a binary granted twice is reviewed once")
	require.Equal(t, "bash:cmd:mkdir", g.rows[0].raw)
	require.Equal(t, "bash:cmd:touch", g.rows[1].raw)
}

func TestGrantsReviewToggleAndApply(t *testing.T) {
	t.Parallel()
	g := newTestGrantsReview(t)

	g.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	g.HandleMsg(keyMsg(' '))
	action := g.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	saved, ok := action.(ActionSaveGrants)
	require.True(t, ok)
	require.Equal(t, []string{"bash:cmd:mkdir", "bash:args:swift build"}, saved.Kept,
		"unchecking touch revokes only that binary")
}

func TestGrantsReviewDigitToggles(t *testing.T) {
	t.Parallel()
	g := newTestGrantsReview(t)
	g.HandleMsg(keyMsg('2'))
	action := g.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	saved, ok := action.(ActionSaveGrants)
	require.True(t, ok)
	require.Equal(t, []string{"bash:cmd:mkdir", "bash:args:swift build"}, saved.Kept,
		"digit 2 unchecks only the touch row")
}

func TestGrantsReviewCloseKeepsAll(t *testing.T) {
	t.Parallel()
	g := newTestGrantsReview(t)
	g.HandleMsg(keyMsg('1')) // uncheck then recheck nothing: toggle off
	g.HandleMsg(keyMsg('1')) // back on
	action := g.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	saved, ok := action.(ActionSaveGrants)
	require.True(t, ok)
	require.Equal(t, []string{"bash:cmd:mkdir", "bash:cmd:touch", "bash:args:swift build"}, saved.Kept,
		"closing without deliberate changes must keep every grant")
}

func TestGrantsReviewMouseClickTogglesRow(t *testing.T) {
	t.Parallel()
	g := newTestGrantsReview(t)
	scr := uv.NewScreenBuffer(80, 30)
	g.Draw(scr, image.Rect(0, 0, 80, 30))
	require.False(t, g.rowsArea.Empty())

	// A click on the "touch" row stages its removal; enter still applies.
	click := tea.MouseClickMsg(tea.Mouse{X: g.rowsArea.Min.X + 1, Y: g.rowsArea.Min.Y + 1, Button: tea.MouseLeft})
	action := g.HandleMsg(click)
	require.Nil(t, action, "clicking only toggles, it never applies")
	require.False(t, g.rows[1].keep)
	require.Equal(t, 1, g.cursor)

	action = g.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	saved, ok := action.(ActionSaveGrants)
	require.True(t, ok)
	require.Equal(t, []string{"bash:cmd:mkdir", "bash:args:swift build"}, saved.Kept)

	// A second click toggles the same row back on.
	g.HandleMsg(click)
	require.True(t, g.rows[1].keep)

	// Clicks outside the row strip change nothing.
	g.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: g.rowsArea.Min.X + 1, Y: g.rowsArea.Min.Y - 2, Button: tea.MouseLeft}))
	require.True(t, g.rows[0].keep)
	require.True(t, g.rows[1].keep)
}

// TestGrantsReviewRowsTrackWrappedHeader pins the click mapping to the
// rendered layout: on a narrow terminal the explainer wraps and the rows
// shift down, and the cached strip must move with them.
func TestGrantsReviewRowsTrackWrappedHeader(t *testing.T) {
	t.Parallel()
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s}
	g := NewGrantsReview(com, []string{"bash:cmd:mkdir", "bash:cmd:touch"})

	scr := uv.NewScreenBuffer(30, 30)
	g.Draw(scr, image.Rect(0, 0, 30, 30))
	narrowTop := g.rowsArea.Min.Y

	g.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: g.rowsArea.Min.X + 1, Y: narrowTop, Button: tea.MouseLeft}))
	require.False(t, g.rows[0].keep, "the strip's first line must be the first row even when the header wraps")
	require.True(t, g.rows[1].keep)
	g.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: g.rowsArea.Min.X + 1, Y: narrowTop, Button: tea.MouseLeft}))

	g.Draw(scr, image.Rect(0, 0, 100, 30))
	require.Less(t, g.rowsArea.Min.Y, narrowTop, "a wider dialog unwraps the explainer and lifts the rows")
	g.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: g.rowsArea.Min.X + 1, Y: g.rowsArea.Min.Y + 1, Button: tea.MouseLeft}))
	require.False(t, g.rows[1].keep)
	require.True(t, g.rows[0].keep)
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
