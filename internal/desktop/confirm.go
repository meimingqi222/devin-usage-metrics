package desktop

import "github.com/egoist/mygo/ui"

// dangerConfirm asks before an action that deletes data or cannot be
// undone. It returns what ui.AlertDialog returns (0 cancel, 1 the action,
// ui.AlertDismissed when Escape or a click outside closes it, -1 while
// unanswered), so call sites switch over as they were.
//
// Two things differ from AlertDialog on purpose. The action button is drawn
// in the theme's danger color: AlertDialog's last button is always the
// accent-blue default, which made "Remove" look like the safe choice. The
// focus starts on Cancel, so Enter pressed out of habit cancels instead of
// deleting.
func dangerConfirm(c *ui.Context, open *bool, title, message, cancel, action string) int {
	if !*open {
		return -1
	}
	t := c.Theme()
	chosen := -1
	ui.DialogBase(c.Key("danger-confirm/"+title), open, func(back, panel ui.Element) {
		back.Background(ui.RGBA(0, 0, 0, .4))
		panel.Width(300).Padding(20).Gap(12).Radius(10).Background(t.Background).
			Shadow(0, 10, 30, 0, ui.RGBA(0, 0, 0, .3)).Role(ui.RoleAlertDialog).Label(title).Description(message)
		ui.Text(c, title).Bold().FontSize(t.FontSize + 1)
		if message != "" {
			ui.Text(c, message).TextColor(t.TextMuted)
		}
		ui.Row(c).Gap(8).Justify(ui.End).Margin(8, 0, 0, 0).Children(func() {
			if ui.Button(c, cancel).AutoFocus().Clicked() {
				chosen = 0
			}
			act := ui.Button(c, action).Background(t.Danger).TextColor(ui.Hex("#ffffff")).Border(0, ui.Color{})
			if act.Hovered() {
				act.Background(t.Danger.Alpha(.85))
			}
			if act.Clicked() {
				chosen = 1
			}
		})
	})
	switch {
	case chosen >= 0:
		*open = false
	case !*open:
		chosen = ui.AlertDismissed
	}
	return chosen
}
