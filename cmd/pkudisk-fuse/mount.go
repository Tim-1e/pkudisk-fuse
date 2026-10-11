//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"pkudisk-fuse/internal/anyshare"
	"pkudisk-fuse/internal/auth"
	"pkudisk-fuse/internal/localfile"
	"pkudisk-fuse/internal/vfs"
)

func mountFileSystem(ctx context.Context, client *anyshare.Client, session *auth.Session, opts mountOptions, output io.Writer) error {
	root, cache := opts.root, localfile.ExpandPath(opts.cache)
	readOnly, debug := opts.readOnly, opts.debug
	cacheLimit, partSize := opts.cacheLimit, opts.partSize
	ttl, timeout, refreshInterval := opts.ttl, opts.timeout, opts.refreshInterval
	client.PartSize = partSize
	var selected *anyshare.Entry
	if root != "" && root != "/" {
		entry, err := client.Resolve(ctx, root)
		if err != nil {
			return fmt.Errorf("select mount root: %w", err)
		}
		if !entry.Dir {
			return syscall.ENOTDIR
		}
		selected = &entry
	} else {
		if _, err := client.Roots(ctx); err != nil {
			return err
		}
	}
	point, err := filepath.Abs(localfile.ExpandPath(opts.mountpoint))
	if err != nil {
		return err
	}
	info, err := os.Stat(point)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return syscall.ENOTDIR
	}
	contents, err := os.ReadDir(point)
	if err != nil {
		return err
	}
	if len(contents) != 0 {
		return errors.New("mountpoint must be an empty directory")
	}
	filesystem, err := vfs.New(client, vfs.Options{Root: selected, ReadOnly: readOnly, CacheDir: cache, CacheLimit: cacheLimit, TTL: ttl, IOTimeout: timeout})
	if err != nil {
		return err
	}
	options := &fs.Options{MountOptions: fuse.MountOptions{Name: "pkudisk-fuse", FsName: "PKU NetDisk", Debug: debug, DisableXAttrs: true, MaxWrite: 1 << 20}, EntryTimeout: &ttl, AttrTimeout: &ttl, NegativeTimeout: new(time.Duration)}
	if readOnly {
		options.MountOptions.Options = []string{"ro"}
	}
	server, err := fs.Mount(point, filesystem.Root(), options)
	if err != nil {
		filesystem.Close()
		return fmt.Errorf("FUSE mount: %w", err)
	}
	fmt.Fprintf(output, "Mounted PKU NetDisk at %s; write cache %s. Ctrl-C unmounts.\n", point, filesystem.CachePath())
	maintenanceCtx, cancelMaintenance := context.WithCancel(ctx)
	maintenanceDone := make(chan struct{})
	maintenanceFailed := make(chan error, 1)
	go func() {
		defer close(maintenanceDone)
		session.KeepAlive(maintenanceCtx, refreshInterval, func(err error) {
			if err == nil {
				fmt.Fprintln(output, "Authentication session renewed.")
			} else if errors.Is(err, auth.ErrSessionExpired) {
				maintenanceFailed <- err
			} else {
				fmt.Fprintf(output, "Session renewal will retry: %v\n", err)
			}
		})
	}()
	defer func() { cancelMaintenance(); <-maintenanceDone }()
	done := make(chan struct{})
	go func() { server.Wait(); close(done) }()
	for {
		select {
		case <-done:
			return filesystem.Close()
		case err := <-maintenanceFailed:
			if unmountErr := server.Unmount(); unmountErr != nil {
				return fmt.Errorf("%w; unmount failed: %w", err, unmountErr)
			}
			<-done
			return errors.Join(err, filesystem.Close())
		case <-ctx.Done():
			if err := server.Unmount(); err != nil {
				return fmt.Errorf("unmount: %w", err)
			}
			<-done
			return filesystem.Close()
		}
	}
}
