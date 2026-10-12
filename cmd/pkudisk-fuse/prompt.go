package main

import (
	"context"
	"errors"
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type credentialPrompt struct {
	input                textinput.Model
	submitted, cancelled bool
}

func newCredentialPrompt(label string, hidden bool) *credentialPrompt {
	input := textinput.New()
	input.Prompt = label
	input.CharLimit = 8192
	input.SetWidth(60)
	if hidden {
		input.EchoMode = textinput.EchoPassword
		input.EchoCharacter = '•'
	}
	return &credentialPrompt{input: input}
}
func (p *credentialPrompt) Init() tea.Cmd { return p.input.Focus() }
func (p *credentialPrompt) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		p.input.SetWidth(max(msg.Width-len(p.input.Prompt)-4, 8))
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			p.cancelled = true
			return p, tea.Quit
		case "enter":
			p.submitted = true
			return p, tea.Quit
		}
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(message)
	return p, cmd
}
func (p *credentialPrompt) View() tea.View { return tea.NewView(p.input.View()) }
func ttyInput(ctx context.Context, label string, hidden bool) ([]byte, error) {
	input, output, err := openTerminal()
	if err != nil {
		return nil, errors.New("interactive terminal unavailable; provide --username and --password-file")
	}
	if input != os.Stdin {
		defer input.Close()
	}
	if output != input && output != os.Stdout && output != os.Stderr {
		defer output.Close()
	}
	prompt := newCredentialPrompt(label, hidden)
	_, err = tea.NewProgram(prompt, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output)).Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if prompt.cancelled || !prompt.submitted {
		return nil, context.Canceled
	}
	value := prompt.input.Value()
	if !hidden {
		value = strings.TrimSpace(value)
	}
	result := []byte(value)
	prompt.input.Reset()
	return result, nil
}
