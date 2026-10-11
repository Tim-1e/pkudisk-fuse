package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
)

type tuiBackend struct {
	entries  []anyshare.Entry
	listErr  error
	download func(context.Context, io.Writer) error
}

func (b *tuiBackend) Roots(context.Context) ([]anyshare.Entry, error) {
	return append([]anyshare.Entry{}, b.entries...), nil
}
func (b *tuiBackend) List(context.Context, string) ([]anyshare.Entry, error) { return nil, b.listErr }
func (b *tuiBackend) Resolve(_ context.Context, selector string) (anyshare.Entry, error) {
	for _, e := range b.entries {
		if e.ID == selector {
			return e, nil
		}
	}
	return anyshare.Entry{}, anyshare.ErrNotFound
}
func (b *tuiBackend) Upload(context.Context, string, string, *anyshare.Entry, io.ReaderAt, int64, time.Time) (anyshare.Entry, error) {
	return anyshare.Entry{}, errors.New("test upload unavailable")
}
func (b *tuiBackend) Download(ctx context.Context, e anyshare.Entry, dest io.Writer) error {
	return b.download(ctx, dest)
}
func finishNetworkCommand(t *testing.T, model *tuiModel, cmd tea.Cmd) {
	t.Helper()
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("network operation not asynchronous")
	}
	model.Update(batch[len(batch)-1]())
}
func TestTUITextRemovesTerminalControls(t *testing.T) {
	if got := tuiText("name\x1b[31m\n"); got != "name?[31m?" {
		t.Fatal(got)
	}
}
func TestNavigationFailurePreservesDirectory(t *testing.T) {
	backend := &tuiBackend{entries: []anyshare.Entry{{ID: "gns://library", Name: "Library", Dir: true}}, listErr: errors.New("network failure")}
	model := newTUIModel(context.Background(), backend, "account", "https://disk.pku.edu.cn", time.Second)
	finishNetworkCommand(t, model, model.Init())
	_, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if model.location.path != "/" || !model.busy {
		t.Fatal("view switched before directory loaded")
	}
	finishNetworkCommand(t, model, cmd)
	if model.location.path != "/" || model.busy || !strings.Contains(model.status, "network failure") || len(model.remote.Items()) != 1 {
		t.Fatal("failed navigation corrupted the browser")
	}
}
func TestFilterDoesNotTriggerUpload(t *testing.T) {
	model := newTUIModel(context.Background(), &tuiBackend{}, "account", "server", time.Second)
	model.remote.SetFilterState(list.Filtering)
	model.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if model.mode != browseMode || model.remote.FilterInput.Value() != "u" {
		t.Fatal("filter keystroke triggered an action")
	}
}
func TestTransferCancellationRemainsResponsive(t *testing.T) {
	backend := &tuiBackend{entries: []anyshare.Entry{{ID: "gns://lib/file", Name: "file.bin", Size: 4}}, download: func(ctx context.Context, dest io.Writer) error { <-ctx.Done(); return ctx.Err() }}
	model := newTUIModel(context.Background(), backend, "account", "server", 5*time.Second)
	command := model.startTransfer("download", "gns://lib/file", t.TempDir(), false)
	batch := command().(tea.BatchMsg)
	done := make(chan tea.Msg, 1)
	go func() { done <- batch[len(batch)-1]() }()
	model.Update(tea.WindowSizeMsg{Width: 45, Height: 15})
	if model.width != 45 || !model.busy {
		t.Fatal("resize was blocked during transfer")
	}
	model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case msg := <-done:
		model.Update(msg)
	case <-time.After(time.Second):
		t.Fatal("cancelled transfer did not exit")
	}
	if model.busy || !strings.Contains(model.status, "cancelled") {
		t.Fatal(model.status)
	}
}
func TestTUIShowsAccountAndResizesList(t *testing.T) {
	model := newTUIModel(context.Background(), &tuiBackend{}, "work", "https://disk.pku.edu.cn", time.Second)
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	view := model.View()
	if !view.AltScreen || !strings.Contains(view.Content, "Account: work") || model.remote.Width() != 56 {
		t.Fatal("missing account or responsive list layout")
	}
}
