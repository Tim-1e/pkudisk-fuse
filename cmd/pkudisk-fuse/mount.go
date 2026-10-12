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
	"github.com/thezzisu/pkudisk-fuse/internal/localfile"
	"github.com/thezzisu/pkudisk-fuse/internal/vfs"
)

func mountFileSystem(ctx context.Context, source *mountSource, opts mountOptions, output io.Writer) error {
	cache := localfile.ExpandPath(opts.cache)
	readOnly, debug := opts.readOnly, opts.debug
	cacheLimit := opts.cacheLimit
	ttl, timeout, refreshInterval := opts.ttl, opts.timeout, opts.refreshInterval
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
	filesystem, err := vfs.New(source.backend, vfs.Options{Root: source.root, ReadOnly: readOnly, CacheDir: cache, CacheLimit: cacheLimit, Read: &opts.read, TTL: ttl, IOTimeout: timeout, Trace: opts.trace})
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
	fmt.Fprintf(output, "Mounted PKU NetDisk at %s; cache %s. Ctrl-C unmounts.\n", point, filesystem.CachePath())
	maintenanceFailed, stopMaintenance := source.maintain(ctx, refreshInterval, output)
	defer stopMaintenance()
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
			opts.trace.Printf(ctx, "unmount requested; waiting for driver to release handles")
			if err := server.Unmount(); err != nil {
				return fmt.Errorf("unmount: %w", err)
			}
			<-done
			return filesystem.Close()
		}
	}
}
