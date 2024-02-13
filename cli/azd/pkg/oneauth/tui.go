// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package oneauth

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	// "github.com/azure/azure-dev/cli/azd/pkg/input" TODO: do something similar with existing functionality?
)

var (
	itemStyle         = lipgloss.NewStyle().PaddingLeft(4)
	selectedItemStyle = lipgloss.NewStyle().PaddingLeft(2).Bold(true)
)

type itemDelegate struct{}

func (itemDelegate) Height() int {
	return 2
}

func (itemDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	a, ok := item.(Account)
	if !ok {
		return
	}
	fn := itemStyle.Render
	str := ""
	if index == m.Index() {
		fn = selectedItemStyle.Render
		str = "❯ "
	}
	str += a.DisplayName
	if a.Username != "" && a.Username != a.DisplayName {
		str += " (" + a.Username + ")"
	}
	// TODO: Office apps seem to associate every account OneAuth knows about, so
	// there's no way within OneAuth to tell which account(s) they have signed in
	// if len(a.AssociatedApps) > 0 {
	// 	names := make([]string, len(a.AssociatedApps))
	// 	for i, app := range a.AssociatedApps {
	// 		names[i] = app[strings.LastIndex(app, ".")+1:]
	// 	}
	// 	str += "\n\tlogged in to: " + strings.Join(names, ", ")
	// }
	fmt.Fprint(w, fn(str))
}

func (itemDelegate) Spacing() int {
	return 0
}

func (itemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd {
	return nil
}

type model struct {
	choice Account
	list   list.Model
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch keypress := msg.String(); keypress {
		case "ctrl+c":
			return m, tea.Quit
		case "enter":
			if a, ok := m.list.SelectedItem().(Account); ok {
				m.choice = a
			}
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.list.SetWidth(msg.Width)
		return m, nil
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) View() string {
	if !m.choice.IsZero() {
		return ""
	}
	return "\n" + m.list.View()
}

func drawAccountPicker(accounts []Account) (Account, error) {
	items := make([]list.Item, len(accounts)+1)
	for i, a := range accounts {
		items[i] = (list.Item)(a)
	}
	items[len(accounts)] = (list.Item)(Account{DisplayName: "Sign in a new account"})

	l := list.New(items, itemDelegate{}, 20, 12)
	l.DisableQuitKeybindings()
	l.SetFilteringEnabled(false)
	l.SetShowHelp(false)
	l.SetShowStatusBar(false)
	l.Styles.Title = lipgloss.NewStyle().MarginLeft(2)
	l.Styles.PaginationStyle = list.DefaultStyles().PaginationStyle.PaddingLeft(4)
	l.Title = "Choose an account"

	m := model{list: l}
	// Run() yields to the scheduler at some point, giving it an opportunity to schedule the calling goroutine on
	// another OS thread. That's problematic if the calling goroutine goes on to call OneAuth's SignInInteractively(),
	// which only works on a UI thread. Apparently we're (always?) on such a thread at this point--perhaps because
	// azd's main goroutine doesn't yield?--so lock to be sure we continue on that thread.
	runtime.LockOSThread()
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return Account{}, err
	}
	runtime.UnlockOSThread()
	choice := final.(model).choice
	if choice.IsZero() {
		// User quit the picker without making a choice (e.g., by pressing Ctrl+C)
		os.Exit(1)
	}
	return choice, nil
}
