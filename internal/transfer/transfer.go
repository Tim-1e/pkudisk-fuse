// Package transfer provides file operations shared by the CLI and TUI. It never
// prints or prompts; callers receive progress and decide how to present it.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pkudisk-fuse/internal/anyshare"
	"pkudisk-fuse/internal/localfile"
)

type Backend interface {
	Resolve(context.Context, string) (anyshare.Entry, error)
	List(context.Context, string) ([]anyshare.Entry, error)
	Upload(context.Context, string, string, *anyshare.Entry, io.ReaderAt, int64, time.Time) (anyshare.Entry, error)
	Download(context.Context, anyshare.Entry, io.Writer) error
}
type Phase string

const (
	Transferring Phase = "transferring"
	Finalizing   Phase = "finalizing"
	Complete     Phase = "complete"
)

type Progress struct {
	Done, Total int64
	Phase       Phase
}
type Observer func(Progress)
type Result struct {
	Entry         anyshare.Entry
	Local, Remote string
}
type Service struct{ Backend Backend }

func (s Service) Upload(ctx context.Context, local, remote string, overwrite bool, observe Observer) (Result, error) {
	local = localfile.ExpandPath(local)
	result := Result{Local: local, Remote: remote}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	file, err := os.Open(local)
	if err != nil {
		return result, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, errors.New("upload source must be a regular file")
	}
	if remote == "" || remote == "/" {
		return result, errors.New("upload destination must be inside a remote library")
	}
	candidate, resolveErr := s.Backend.Resolve(ctx, remote)
	var parent, name string
	var existing *anyshare.Entry
	if resolveErr == nil {
		if candidate.Dir {
			parent = candidate.ID
			name = filepath.Base(local)
			entries, err := s.Backend.List(ctx, parent)
			if err != nil {
				return result, err
			}
			for _, entry := range entries {
				if entry.Name == name {
					copy := entry
					existing = &copy
					break
				}
			}
		} else {
			parent = anyshare.ParentID(candidate.ID)
			name = candidate.Name
			existing = &candidate
		}
	} else if errors.Is(resolveErr, anyshare.ErrNotFound) {
		parentSelector, fileName, err := remoteParent(remote)
		if err != nil {
			return result, err
		}
		folder, err := s.Backend.Resolve(ctx, parentSelector)
		if err != nil {
			return result, fmt.Errorf("resolve upload parent: %w", err)
		}
		if !folder.Dir {
			return result, errors.New("upload parent is not a remote folder")
		}
		parent = folder.ID
		name = fileName
	} else {
		return result, resolveErr
	}
	if parent == "" || !anyshare.ValidName(name) {
		return result, errors.New("invalid upload destination or file name")
	}
	if existing != nil {
		if existing.Dir {
			return result, errors.New("a remote folder already has that name")
		}
		if !overwrite {
			return result, fmt.Errorf("%w: %q; use --overwrite to replace it", anyshare.ErrExists, name)
		}
	}
	progress := newReporter(info.Size(), observe)
	entry, err := s.Backend.Upload(ctx, parent, name, existing, progressReaderAt{file, progress}, info.Size(), info.ModTime())
	if err != nil {
		return result, err
	}
	result.Entry = entry
	progress.complete()
	return result, nil
}

func (s Service) Download(ctx context.Context, remote, local string, overwrite bool, observe Observer) (Result, error) {
	entry, err := s.Backend.Resolve(ctx, remote)
	if err != nil {
		return Result{Remote: remote, Local: local}, err
	}
	result, err := s.DownloadEntry(ctx, entry, local, overwrite, observe)
	result.Remote = remote
	return result, err
}
func (s Service) DownloadEntry(ctx context.Context, entry anyshare.Entry, local string, overwrite bool, observe Observer) (Result, error) {
	local = localfile.ExpandPath(local)
	result := Result{Entry: entry, Local: local, Remote: entry.ID}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if entry.Dir {
		return result, errors.New("download source is a folder; select a single remote file")
	}
	if !anyshare.ValidName(entry.Name) || entry.Size < 0 {
		return result, errors.New("invalid remote file name or size")
	}
	if local == "" {
		if !localfile.ValidName(entry.Name) {
			return result, errors.New("remote name cannot be used as a local filename on this platform; provide an explicit local file path")
		}
		local = entry.Name
	}
	if info, err := os.Stat(local); err == nil && info.IsDir() {
		if !localfile.ValidName(entry.Name) {
			return result, errors.New("remote name cannot be used as a local filename on this platform; provide an explicit local file path")
		}
		local = filepath.Join(local, entry.Name)
	}
	result.Local = local
	if _, err := os.Lstat(local); err == nil && !overwrite {
		return result, fmt.Errorf("%w: %s; use --overwrite to replace it", os.ErrExist, local)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	file, err := os.CreateTemp(filepath.Dir(local), ".pkudisk-download-*")
	if err != nil {
		return result, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	progress := newReporter(entry.Size, observe)
	if err = s.Backend.Download(ctx, entry, progressWriter{file, progress}); err != nil {
		return result, err
	}
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	if info.Size() != entry.Size {
		return result, errors.New("download size differs from the selected file")
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = file.Sync(); err != nil {
		return result, err
	}
	if err = file.Close(); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	err = localfile.Install(file.Name(), local, overwrite)
	if err != nil {
		return result, err
	}
	progress.complete()
	return result, nil
}

func remoteParent(selector string) (string, string, error) {
	selector = strings.TrimRight(selector, "/")
	at := strings.LastIndex(selector, "/")
	if at < 0 {
		return "", "", errors.New("upload destination must include an accessible library")
	}
	parent, name := selector[:at], selector[at+1:]
	if !anyshare.ValidName(name) || parent == "" || parent == "gns:" || parent == "gns:/" {
		return "", "", errors.New("invalid remote upload destination")
	}
	return parent, name, nil
}

type reporter struct {
	mu      sync.Mutex
	state   Progress
	observe Observer
}

func newReporter(total int64, observe Observer) *reporter {
	r := &reporter{state: Progress{Total: total, Phase: Transferring}, observe: observe}
	if total == 0 {
		r.state.Phase = Finalizing
	}
	if observe != nil {
		observe(r.state)
	}
	return r
}
func (r *reporter) add(n int) {
	if n <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Phase == Complete {
		return
	}
	r.state.Done += min(int64(n), r.state.Total-r.state.Done)
	if r.state.Done == r.state.Total {
		r.state.Phase = Finalizing
	}
	if r.observe != nil {
		r.observe(r.state)
	}
}
func (r *reporter) complete() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Done = r.state.Total
	r.state.Phase = Complete
	if r.observe != nil {
		r.observe(r.state)
	}
}

type progressReaderAt struct {
	io.ReaderAt
	progress *reporter
}

func (r progressReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, off)
	r.progress.add(n)
	return n, err
}

type progressWriter struct {
	io.Writer
	progress *reporter
}

func (w progressWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.progress.add(n)
	return n, err
}
