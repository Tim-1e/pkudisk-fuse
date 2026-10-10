package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"pkudisk-fuse/internal/anyshare"
)

func remoteParent(selector string) (string, string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" || selector == "/" {
		return "", "", errors.New("remote destination must include a file name")
	}
	clean := strings.Trim(selector, "/")
	if clean == "" || clean == "." || clean == ".." {
		return "", "", errors.New("invalid remote destination")
	}
	name := path.Base(clean)
	parent := path.Dir(clean)
	if !anyshare.ValidName(name) {
		return "", "", errors.New("remote destination has an invalid file name")
	}
	if parent == "." {
		return "", "", errors.New("remote destination must include an accessible library")
	}
	if strings.HasPrefix(clean, "gns://") {
		// Preserve the scheme's double slash while removing only a trailing slash.
		idx := strings.LastIndex(clean, "/")
		parent, name = clean[:idx], clean[idx+1:]
		if !strings.HasPrefix(parent, "gns://") {
			return "", "", errors.New("invalid gns remote destination")
		}
	}
	return parent, name, nil
}

func uploadFile(ctx context.Context, client *anyshare.Client, local, remote string, overwrite bool) error {
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
	var existing *anyshare.Entry
	remoteSelector := strings.Trim(remote, "/")
	candidate, resolveErr := client.Resolve(ctx, remoteSelector)
	parentSelector, name := "", ""
	if resolveErr == nil {
		if candidate.Dir {
			// A folder destination follows the usual command-line convention:
			// upload the source under its local base name.
			parentSelector = remoteSelector
			name = path.Base(local)
			if !anyshare.ValidName(name) {
				return errors.New("upload source has an invalid file name")
			}
			targetSelector := remoteSelector + "/" + name
			if target, targetErr := client.Resolve(ctx, targetSelector); targetErr == nil {
				if target.Dir {
					return errors.New("upload destination is a folder")
				}
				if !overwrite {
					return fmt.Errorf("%w: use --overwrite to replace %q", anyshare.ErrExists, target.Name)
				}
				existing = &target
			} else if !errors.Is(targetErr, anyshare.ErrNotFound) {
				return fmt.Errorf("check upload destination: %w", targetErr)
			}
		} else {
			parentSelector, name, err = remoteParent(remote)
			if err != nil {
				return err
			}
		}
		if !candidate.Dir {
			if !overwrite {
				return fmt.Errorf("%w: use --overwrite to replace %q", anyshare.ErrExists, candidate.Name)
			}
			existing = &candidate
		}
	} else if errors.Is(resolveErr, anyshare.ErrNotFound) {
		parentSelector, name, err = remoteParent(remote)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("check upload destination: %w", resolveErr)
	}
	parent, err := client.Resolve(ctx, parentSelector)
	if err != nil {
		return fmt.Errorf("resolve upload destination: %w", err)
	}
	if !parent.Dir {
		return errors.New("upload destination parent is not a folder")
	}
	progress := newTransferProgress("Upload", info.Size())
	entry, err := client.Upload(ctx, parent.ID, name, existing, progressReaderAt{reader: file, progress: progress}, info.Size(), info.ModTime())
	progress.finish(err)
	if err != nil {
		return fmt.Errorf("upload %q: %w", local, err)
	}
	fmt.Fprintf(os.Stdout, "Uploaded %s -> %s (%d bytes)\n", local, remote, entry.Size)
	return nil
}

func downloadFile(ctx context.Context, client *anyshare.Client, remote, local string, overwrite bool) error {
	entry, err := client.Resolve(ctx, remote)
	if err != nil {
		return fmt.Errorf("resolve download source: %w", err)
	}
	if entry.Dir {
		return errors.New("download source is a folder")
	}
	if local == "" {
		local = path.Base(strings.Trim(remote, "/"))
		if local == "." || local == "" || local == "/" {
			local = entry.Name
		}
	}
	if info, statErr := os.Stat(local); statErr == nil && info.IsDir() {
		local = path.Join(local, entry.Name)
	}
	return downloadTo(ctx, client, entry, local, overwrite)
}

func downloadTo(ctx context.Context, client *anyshare.Client, entry anyshare.Entry, destination string, overwrite bool) (result error) {
	dir := path.Dir(destination)
	base := path.Base(destination)
	if base == "." || base == "/" || !anyshare.ValidName(base) {
		return errors.New("invalid local download destination")
	}
	if dir == "" {
		dir = "."
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		if err != nil {
			return err
		}
		return errors.New("local download parent is not a directory")
	}
	if _, err := os.Lstat(destination); err == nil && !overwrite {
		return fmt.Errorf("local destination already exists: %s (use --overwrite)", destination)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+base+".pkudisk-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	defer tmp.Close()
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	progress := newTransferProgress("Download", entry.Size)
	defer func() { progress.finish(result) }()
	if err = client.Download(ctx, entry, progressWriter{writer: tmp, progress: progress}); err != nil {
		return fmt.Errorf("download %q: %w", entry.Name, err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if overwrite {
		// Rename replaces a symlink itself, never the file it points to.
		err = os.Rename(tmpName, destination)
	} else {
		// Linking the finished file also checks for a destination created while
		// the download was in flight, without overwriting it.
		err = os.Link(tmpName, destination)
	}
	if err != nil {
		return err
	}
	progress.finish(nil)
	fmt.Fprintf(os.Stdout, "Downloaded %s -> %s (%d bytes)\n", entry.Name, destination, entry.Size)
	return nil
}
