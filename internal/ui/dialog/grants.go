package dialog

import (
	"fmt"
	"image"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/ui/common"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// GrantsReviewID is the identifier for the saved command grants review
// dialog.
const GrantsReviewID = "grants_review"

// ActionSaveGrants reports the scoped grant entries the user kept. The
// caller persists the workspace config so unchecked entries stop
// pre-approving from the next request onward.
type ActionSaveGrants struct {
	// Kept holds the raw entries (e.g. "bash:cmd:mkdir") that remain
	// allowed, in display order, flattened to one command per cmd-tier
	// entry.
	Kept []string
}

// grantRow is one parsed entry in the review list. Cmd rows carry a single
// binary; args rows carry one verbatim cmd+args shape.
type grantRow struct {
	raw   string // e.g. "bash:cmd:mkdir"
	label string // e.g. "bash cmd  mkdir"
	keep  bool
}

// GrantsReview lists the scoped allowed_tools entries loaded from config at
// startup. Everything starts checked; the user unchecks grants they do not
// want active this session.
type GrantsReview struct {
	com    *common.Common
	help   help.Model
	rows   []grantRow
	cursor int
	// rowsArea is the on-screen strip of grant rows, cached at draw time
	// so mouse clicks resolve to the row the user sees.
	rowsArea image.Rectangle
	keyMap   struct{ Up, Down, Toggle, Done, Close key.Binding }
}

var _ Dialog = (*GrantsReview)(nil)

// NewGrantsReview builds the review dialog from raw allowed_tools entries.
// Entries that are not scoped grants (plain "bash", "view", ...) are
// filtered out: they are config-level tool allowances, not reviewed here.
// Joined cmd entries are flattened to one row per binary, so every command
// can be kept or revoked on its own, and duplicates collapse.
func NewGrantsReview(com *common.Common, entries []string) *GrantsReview {
	g := &GrantsReview{com: com}
	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	g.help = h
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		for _, flat := range permission.FlattenScopedEntry(e) {
			if seen[flat] {
				continue
			}
			tool, subject, ok := permission.CutScopedEntry(flat)
			if !ok {
				continue
			}
			seen[flat] = true
			tier := "cmd"
			scope := subject
			if rest, isArgs := strings.CutPrefix(subject, permission.ScopeArgs); isArgs {
				tier = "args"
				scope = rest
			} else if rest, isCmd := strings.CutPrefix(subject, permission.ScopeCmd); isCmd {
				scope = rest
			}
			g.rows = append(g.rows, grantRow{
				raw:   flat,
				label: fmt.Sprintf("%s %s  %s", tool, tier, scope),
				keep:  true,
			})
		}
	}
	g.keyMap.Up = key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "move"))
	g.keyMap.Down = key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "move"))
	g.keyMap.Toggle = key.NewBinding(key.WithKeys(" ", "space"), key.WithHelp("space", "toggle"))
	g.keyMap.Done = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply"))
	g.keyMap.Close = CloseKey
	return g
}

// HasRows reports whether there is anything to review.
func (g *GrantsReview) HasRows() bool {
	return len(g.rows) > 0
}

// ID implements [Dialog].
func (*GrantsReview) ID() string {
	return GrantsReviewID
}

// HandleMsg implements [Dialog].
func (g *GrantsReview) HandleMsg(msg tea.Msg) Action {
	if len(g.rows) == 0 {
		return ActionClose{}
	}
	if click, ok := msg.(tea.MouseClickMsg); ok {
		return g.handleMouseClick(click)
	}
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch {
	case key.Matches(keyMsg, g.keyMap.Close):
		// Closing keeps everything: nothing was revoked without a
		// deliberate uncheck.
		return ActionSaveGrants{Kept: g.kept()}
	case key.Matches(keyMsg, g.keyMap.Up):
		g.cursor = (g.cursor - 1 + len(g.rows)) % len(g.rows)
	case key.Matches(keyMsg, g.keyMap.Down):
		g.cursor = (g.cursor + 1) % len(g.rows)
	case key.Matches(keyMsg, g.keyMap.Toggle):
		g.rows[g.cursor].keep = !g.rows[g.cursor].keep
	case key.Matches(keyMsg, g.keyMap.Done):
		return ActionSaveGrants{Kept: g.kept()}
	default:
		if idx := digitIndex(keyMsg.String()); idx >= 0 && idx < len(g.rows) {
			g.cursor = idx
			g.rows[idx].keep = !g.rows[idx].keep
		}
	}
	return nil
}

// handleMouseClick toggles the checkbox of the row under the pointer,
// matching the digit-key behavior: the click stages the change and enter
// still applies it.
func (g *GrantsReview) handleMouseClick(msg tea.MouseClickMsg) Action {
	if msg.Button != tea.MouseLeft || !image.Pt(msg.X, msg.Y).In(g.rowsArea) {
		return nil
	}
	idx := msg.Y - g.rowsArea.Min.Y
	if idx < 0 || idx >= len(g.rows) {
		return nil
	}
	g.cursor = idx
	g.rows[idx].keep = !g.rows[idx].keep
	return nil
}

func (g *GrantsReview) kept() []string {
	kept := make([]string, 0, len(g.rows))
	for _, row := range g.rows {
		if row.keep {
			kept = append(kept, row.raw)
		}
	}
	return kept
}

// digitIndex maps a numeric keypress to a list index, -1 when the key is
// not a digit.
func digitIndex(s string) int {
	if len(s) != 1 || s[0] < '0' || s[0] > '9' {
		return -1
	}
	return int(s[0] - '1')
}

// Draw implements [Dialog].
func (g *GrantsReview) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := g.com.Styles
	width := min(area.Dx()-4, defaultDialogMaxWidth)
	dialogStyle := t.Dialog.View.Width(width).Padding(0, 1)
	contentWidth := width - dialogStyle.GetHorizontalFrameSize()

	title := common.DialogTitle(t, "Saved Command Grants", contentWidth, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)
	title = t.Dialog.Title.Render(title)

	lines := []string{
		title,
		"",
		t.Dialog.Permissions.ValueText.Render("Pre-approved from your workspace config. Uncheck to revoke this session."),
		"",
	}
	for i, row := range g.rows {
		mark := "[ ]"
		if row.keep {
			mark = "[x]"
		}
		text := fmt.Sprintf("%d %s %s", i+1, mark, row.label)
		text = ansi.Truncate(text, contentWidth, "…")
		style := t.Dialog.NormalItem
		if i == g.cursor {
			style = t.Dialog.SelectedItem
		}
		lines = append(lines, style.Render(text))
	}

	helpView := shortHelpLine(&g.help, g.ShortHelp(), contentWidth)
	content := lipgloss.JoinVertical(lipgloss.Left, lines...)
	content = lipgloss.JoinVertical(lipgloss.Left, content, "", helpView)

	view := dialogStyle.Render(content)
	vw, vh := lipgloss.Size(view)
	vw = min(vw, area.Dx())
	vh = min(vh, area.Dy())
	rect := common.CenterRect(area, vw, vh)
	// Rows sit below the four header lines (title, blank, explainer,
	// blank) inside the frame; cache that strip for click hit-testing.
	left := rect.Min.X + dialogStyle.GetHorizontalFrameSize()/2
	top := rect.Min.Y + dialogStyle.GetVerticalFrameSize()/2 + 4
	g.rowsArea = image.Rect(left, top, left+contentWidth, top+len(g.rows))

	uv.NewStyledString(view).Draw(scr, rect)
	return nil
}

// ShortHelp implements [help.KeyMap].
func (g *GrantsReview) ShortHelp() []key.Binding {
	return []key.Binding{g.keyMap.Up, g.keyMap.Down, g.keyMap.Toggle, g.keyMap.Done, g.keyMap.Close}
}
