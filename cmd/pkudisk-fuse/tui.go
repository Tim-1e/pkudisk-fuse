package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"pkudisk-fuse/internal/anyshare"
)

type tui struct {
	ctx         context.Context
	client      *anyshare.Client
	timeout     time.Duration
	fd          int
	raw         *term.State
	current     *anyshare.Entry
	currentPath string
	entries     []anyshare.Entry
	cursor      int
	status      string
}

func runTUI(ctx context.Context, client *anyshare.Client, timeout time.Duration) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("tui requires an interactive terminal")
	}
	ui := &tui{ctx: ctx, client: client, timeout: timeout, fd: fd, currentPath: "/"}
	if err := ui.load(); err != nil {
		return err
	}
	raw, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("enter raw terminal mode: %w", err)
	}
	ui.raw = raw
	defer func() {
		_ = term.Restore(fd, raw)
		fmt.Fprint(os.Stdout, "\x1b[0m\r\n")
	}()
	return ui.loop()
}

func (ui *tui) load() error {
	var entries []anyshare.Entry
	var err error
	if ui.current == nil {
		entries, err = ui.client.Roots(ui.ctx)
	} else {
		entries, err = ui.client.List(ui.ctx, ui.current.ID)
	}
	if err != nil {
		return err
	}
	ui.entries = entries
	if ui.cursor >= len(entries) {
		ui.cursor = len(entries) - 1
	}
	if ui.cursor < 0 {
		ui.cursor = 0
	}
	return nil
}

func (ui *tui) selected() *anyshare.Entry {
	if ui.cursor < 0 || ui.cursor >= len(ui.entries) {
		return nil
	}
	return &ui.entries[ui.cursor]
}

func (ui *tui) render() {
	fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")
	fmt.Fprintf(os.Stdout, "PKU NetDisk  %s\r\n", tuiText(ui.currentPath))
	fmt.Fprint(os.Stdout, "j/k or arrows: move   Enter: open   Backspace: parent   u: upload   d: download   r: refresh   q: quit\r\n\r\n")
	if len(ui.entries) == 0 {
		fmt.Fprint(os.Stdout, "(empty)\r\n")
	} else {
		for i, entry := range ui.entries {
			marker := "  "
			if i == ui.cursor {
				marker = "> "
			}
			typ := "FILE"
			size := fmt.Sprintf("%d B", entry.Size)
			if entry.Dir {
				typ, size = "DIR ", ""
			}
			fmt.Fprintf(os.Stdout, "%s%-4s %-12s %s\r\n", marker, typ, size, tuiText(entry.Name))
		}
	}
	if ui.status != "" {
		fmt.Fprintf(os.Stdout, "\r\n%s\r\n", tuiText(ui.status))
		ui.status = ""
	}
}

func tuiText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, value)
}

func (ui *tui) loop() error {
	for {
		ui.render()
		key, err := tuiReadKey(ui.fd)
		if err != nil {
			return err
		}
		switch key {
		case 'q', 'Q', 3:
			return nil
		case 'j', 'J':
			ui.move(1)
		case 'k', 'K':
			ui.move(-1)
		case 'r', 'R':
			if err := ui.load(); err != nil {
				ui.status = "refresh: " + err.Error()
			}
		case '\r', '\n':
			if err := ui.enter(); err != nil {
				ui.status = err.Error()
			}
		case 8, 127, 'h', 'H':
			if err := ui.parent(); err != nil {
				ui.status = err.Error()
			}
		case 'u', 'U':
			if err := ui.upload(); err != nil {
				if !errors.Is(err, io.EOF) {
					ui.status = "upload: " + err.Error()
				}
			}
		case 'd', 'D':
			if err := ui.download(); err != nil {
				if !errors.Is(err, io.EOF) {
					ui.status = "download: " + err.Error()
				}
			}
		case 'g', 'G':
			ui.current = nil
			ui.currentPath = "/"
			ui.cursor = 0
			if err := ui.load(); err != nil {
				ui.status = "root: " + err.Error()
			}
		case '?':
			ui.status = "Enter opens a folder; u uploads into it; d downloads the selected file."
		}
	}
}

func (ui *tui) move(delta int) {
	if len(ui.entries) == 0 {
		return
	}
	ui.cursor += delta
	if ui.cursor < 0 {
		ui.cursor = len(ui.entries) - 1
	}
	if ui.cursor >= len(ui.entries) {
		ui.cursor = 0
	}
}

func (ui *tui) enter() error {
	entry := ui.selected()
	if entry == nil {
		return errors.New("no remote entry selected")
	}
	if !entry.Dir {
		return errors.New("selected entry is a file; press d to download it")
	}
	copy := *entry
	ui.current = &copy
	if ui.currentPath == "/" {
		ui.currentPath = "/" + entry.Name
	} else {
		ui.currentPath = path.Join(ui.currentPath, entry.Name)
	}
	ui.cursor = 0
	return ui.load()
}

func (ui *tui) parent() error {
	if ui.current == nil {
		return errors.New("already at the remote root")
	}
	parentID := anyshare.ParentID(ui.current.ID)
	parentPath := path.Dir(ui.currentPath)
	if parentID == "" || parentPath == "." || parentPath == "/" {
		ui.current = nil
		ui.currentPath = "/"
	} else {
		ui.current = &anyshare.Entry{ID: parentID, Name: path.Base(parentPath), Dir: true, Size: -1}
		ui.currentPath = parentPath
	}
	ui.cursor = 0
	return ui.load()
}

func (ui *tui) prompt(label, defaultValue string) (string, error) {
	if err := term.Restore(ui.fd, ui.raw); err != nil {
		return "", err
	}
	if defaultValue != "" {
		fmt.Fprintf(os.Stdout, "\r\n%s [%s]: ", label, defaultValue)
	} else {
		fmt.Fprintf(os.Stdout, "\r\n%s: ", label)
	}
	line, readErr := bufio.NewReader(os.Stdin).ReadString('\n')
	raw, rawErr := term.MakeRaw(ui.fd)
	if rawErr == nil {
		ui.raw = raw
	}
	if readErr != nil {
		return "", readErr
	}
	if rawErr != nil {
		return "", rawErr
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = defaultValue
	}
	return line, nil
}

func expandTUIPath(value string) string {
	if value != "~" && !strings.HasPrefix(value, "~/") {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return value
	}
	if value == "~" {
		return home
	}
	return filepath.Join(home, strings.TrimPrefix(value, "~/"))
}

func (ui *tui) upload() error {
	if ui.current == nil {
		return errors.New("enter a library or folder before uploading")
	}
	local, err := ui.prompt("Local file to upload", "")
	if err != nil || local == "" {
		return err
	}
	local = expandTUIPath(local)
	file, err := os.Open(local)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("upload source must be a regular file")
	}
	name := filepath.Base(local)
	if !anyshare.ValidName(name) {
		return errors.New("upload source has an invalid file name")
	}
	entries, err := ui.client.List(ui.ctx, ui.current.ID)
	if err != nil {
		return err
	}
	var existing *anyshare.Entry
	for i := range entries {
		if entries[i].Name != name {
			continue
		}
		if entries[i].Dir {
			return errors.New("a folder with that name already exists")
		}
		answer, promptErr := ui.prompt("Overwrite remote "+name+"? [y/N]", "")
		if promptErr != nil {
			return promptErr
		}
		if strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes" {
			return nil
		}
		candidate := entries[i]
		existing = &candidate
		break
	}
	opCtx, cancel := context.WithTimeout(ui.ctx, ui.timeout)
	defer cancel()
	progress := newTransferProgress("Upload", info.Size())
	entry, err := ui.client.Upload(opCtx, ui.current.ID, name, existing, progressReaderAt{reader: file, progress: progress}, info.Size(), info.ModTime())
	progress.finish(err)
	if err != nil {
		return err
	}
	ui.status = fmt.Sprintf("uploaded %s (%d bytes)", entry.Name, entry.Size)
	return ui.load()
}

func (ui *tui) download() error {
	entry := ui.selected()
	if entry == nil {
		return errors.New("no remote entry selected")
	}
	if entry.Dir {
		return errors.New("selected entry is a folder; press Enter to open it")
	}
	defaultPath := filepath.Join(".", entry.Name)
	destination, err := ui.prompt("Local destination", defaultPath)
	if err != nil {
		return err
	}
	destination = expandTUIPath(destination)
	if info, statErr := os.Stat(destination); statErr == nil && info.IsDir() {
		destination = filepath.Join(destination, entry.Name)
	}
	overwrite := false
	if _, statErr := os.Lstat(destination); statErr == nil {
		answer, promptErr := ui.prompt("Overwrite local "+destination+"? [y/N]", "")
		if promptErr != nil {
			return promptErr
		}
		overwrite = strings.ToLower(answer) == "y" || strings.ToLower(answer) == "yes"
		if !overwrite {
			return nil
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	opCtx, cancel := context.WithTimeout(ui.ctx, ui.timeout)
	defer cancel()
	err = downloadTo(opCtx, ui.client, *entry, destination, overwrite)
	if err == nil {
		ui.status = fmt.Sprintf("downloaded %s", entry.Name)
	}
	return err
}

func tuiReadKey(fd int) (byte, error) {
	var one [1]byte
	if _, err := unix.Read(fd, one[:]); err != nil {
		return 0, err
	}
	if one[0] != 0x1b {
		return one[0], nil
	}
	second, ok, err := tuiPollByte(fd, 75*time.Millisecond)
	if err != nil || !ok || second != '[' {
		return 0, err
	}
	third, ok, err := tuiPollByte(fd, 75*time.Millisecond)
	if err != nil || !ok {
		return 0, err
	}
	switch third {
	case 'A':
		return 'k', nil
	case 'B':
		return 'j', nil
	default:
		return 0, nil
	}
}

func tuiPollByte(fd int, timeout time.Duration) (byte, bool, error) {
	millis := int(timeout / time.Millisecond)
	if millis < 1 {
		millis = 1
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(poll, millis)
	if err != nil {
		return 0, false, err
	}
	if n == 0 {
		return 0, false, nil
	}
	var one [1]byte
	if _, err = unix.Read(fd, one[:]); err != nil {
		return 0, false, err
	}
	return one[0], true, nil
}
