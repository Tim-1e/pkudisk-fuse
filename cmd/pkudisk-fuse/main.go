package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"pkudisk-fuse/internal/anyshare"
	"pkudisk-fuse/internal/auth"
	"pkudisk-fuse/internal/vfs"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pkudisk-fuse:", err)
		os.Exit(1)
	}
}
func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  pkudisk-fuse mount [options] MOUNTPOINT
  pkudisk-fuse root [authentication options] [--json]
  pkudisk-fuse ls [authentication options] [--json] [PATH-OR-GNS-ID]
  pkudisk-fuse tui [authentication options]
  pkudisk-fuse account [list|add|default|remove] [options]
  pkudisk-fuse upload [authentication options] LOCAL-PATH REMOTE-PATH
  pkudisk-fuse download [authentication options] REMOTE-PATH [LOCAL-PATH]
  pkudisk-fuse recover [--cache-dir PATH] [--json]
  pkudisk-fuse version

Commands reuse the default saved account. Use --account NAME to select one,
or --username and a hidden password prompt, --password-file,
--password-stdin, or --credentials-file for a login.
--root '/LIBRARY/FOLDER' or --root 'gns://...' selects a library/folder;
omitting it mounts all accessible libraries. Run a command with --help for options.`)
}

func defaultCache() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "pkudisk-fuse")
}
func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("a command is required")
	}
	switch args[0] {
	case "version":
		fmt.Println("pkudisk-fuse", version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	case "recover":
		return recovery(args[1:])
	case "account":
		return accountCommand(args[1:])
	case "mount", "root", "ls", "tui", "upload", "download":
	default:
		usage()
		return errors.New("unknown command")
	}
	command := args[0]
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	var credentials authFlags
	credentials.add(set)
	var root, cache, output string
	var readOnly, debug, jsonOutput, overwrite bool
	var cacheLimit, partSize int64
	var ttl, timeout, refreshInterval time.Duration
	if command == "mount" {
		set.StringVar(&root, "root", "", "mount only this library/folder path or gns:// ID")
		set.StringVar(&cache, "cache-dir", defaultCache(), "private local write-cache directory (0700)")
		set.Int64Var(&cacheLimit, "cache-limit", 10<<30, "maximum total write-cache bytes; 0 means unlimited")
		set.Int64Var(&partSize, "part-size", 16<<20, "multipart upload chunk bytes (minimum 5 MiB)")
		set.DurationVar(&ttl, "cache-ttl", 2*time.Second, "directory and kernel attribute cache lifetime")
		set.DurationVar(&timeout, "io-timeout", 10*time.Minute, "download/upload timeout per file operation")
		set.DurationVar(&refreshInterval, "refresh-interval", time.Hour, "renew idle authentication sessions at this interval; 0 uses refresh on HTTP 401 only")
		set.BoolVar(&readOnly, "read-only", false, "mount read-only")
		set.BoolVar(&debug, "debug", false, "enable FUSE protocol diagnostics")
	} else if command == "root" || command == "ls" {
		set.BoolVar(&jsonOutput, "json", false, "output entries as JSON")
	} else if command == "tui" {
		set.DurationVar(&timeout, "io-timeout", 10*time.Minute, "upload/download timeout")
	} else if command == "upload" {
		set.StringVar(&output, "remote", "", "remote destination path (also accepted as the second argument)")
		set.BoolVar(&overwrite, "overwrite", false, "replace an existing remote file")
		set.DurationVar(&timeout, "io-timeout", 10*time.Minute, "upload timeout")
	} else {
		set.StringVar(&output, "output", "", "local output file (defaults to the remote file name)")
		set.BoolVar(&overwrite, "overwrite", false, "replace an existing local file")
		set.DurationVar(&timeout, "io-timeout", 10*time.Minute, "download timeout")
	}
	if err := set.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if command == "mount" && set.NArg() != 1 {
		return errors.New("mount requires one mountpoint; place options before it")
	}
	if command == "ls" && set.NArg() > 1 {
		return errors.New("ls accepts at most one library/folder path or gns:// ID")
	}
	if command == "root" && set.NArg() != 0 {
		return errors.New("root takes no positional arguments")
	}
	if command == "tui" && set.NArg() != 0 {
		return errors.New("tui takes no positional arguments")
	}
	if command == "upload" && (set.NArg() < 1 || set.NArg() > 2) {
		return errors.New("upload requires LOCAL-PATH and REMOTE-PATH (or --remote REMOTE-PATH)")
	}
	if command == "upload" && output != "" && set.NArg() == 2 {
		return errors.New("upload accepts the remote destination either as the second argument or with --remote, not both")
	}
	if command == "download" && (set.NArg() < 1 || set.NArg() > 2) {
		return errors.New("download requires REMOTE-PATH and optional LOCAL-PATH")
	}
	if command == "download" && output != "" && set.NArg() == 2 {
		return errors.New("download accepts the local destination either as the second argument or with --output, not both")
	}
	if command == "mount" && (cacheLimit < 0 || partSize < 5<<20 || ttl < 0 || timeout <= 0 || refreshInterval < 0) {
		return errors.New("invalid cache, part-size, or timeout option")
	}
	if (command == "upload" || command == "download" || command == "tui") && timeout <= 0 {
		return errors.New("invalid I/O timeout")
	}
	ctx := context.Background()
	credentials.announce = command != "tui"
	session, serverURL, _, err := authenticate(ctx, credentials)
	if err != nil {
		return err
	}
	defer session.Close()
	client, err := anyshare.New(serverURL, session)
	if err != nil {
		return err
	}
	if command == "root" {
		entries, err := client.Roots(ctx)
		if err != nil {
			return err
		}
		return printEntries(entries, jsonOutput)
	}
	if command == "upload" {
		local := set.Arg(0)
		remote := output
		if remote == "" && set.NArg() == 2 {
			remote = set.Arg(1)
		}
		if remote == "" {
			return errors.New("upload requires a remote destination path")
		}
		transferCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return uploadFile(transferCtx, client, local, remote, overwrite)
	}
	if command == "download" {
		remote := set.Arg(0)
		local := output
		if local == "" && set.NArg() == 2 {
			local = set.Arg(1)
		}
		transferCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return downloadFile(transferCtx, client, remote, local, overwrite)
	}
	if command == "tui" {
		return runTUI(ctx, client, timeout)
	}
	if command == "ls" {
		var entries []anyshare.Entry
		selector := "/"
		if set.NArg() == 1 {
			selector = set.Arg(0)
		}
		if selector == "/" {
			entries, err = client.Roots(ctx)
		} else {
			var e anyshare.Entry
			e, err = client.Resolve(ctx, selector)
			if err == nil {
				if !e.Dir {
					return syscall.ENOTDIR
				}
				entries, err = client.List(ctx, e.ID)
			}
		}
		if err != nil {
			return err
		}
		return printEntries(entries, jsonOutput)
	}
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
	point, err := filepath.Abs(set.Arg(0))
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
	fmt.Fprintf(os.Stderr, "Mounted PKU NetDisk at %s; write cache %s. Ctrl-C unmounts.\n", point, filesystem.CachePath())
	maintenanceCtx, cancelMaintenance := context.WithCancel(ctx)
	maintenanceDone := make(chan struct{})
	maintenanceFailed := make(chan error, 1)
	go func() {
		defer close(maintenanceDone)
		session.KeepAlive(maintenanceCtx, refreshInterval, func(err error) {
			if err == nil {
				fmt.Fprintln(os.Stderr, "Authentication session renewed.")
			} else if errors.Is(err, auth.ErrSessionExpired) {
				maintenanceFailed <- err
			} else {
				fmt.Fprintf(os.Stderr, "Session renewal will retry: %v\n", err)
			}
		})
	}()
	defer func() { cancelMaintenance(); <-maintenanceDone }()
	done := make(chan struct{})
	go func() { server.Wait(); close(done) }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
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
		case <-signals:
			if err := server.Unmount(); err != nil {
				fmt.Fprintln(os.Stderr, "Unmount failed:", err)
				continue
			}
			<-done
			return filesystem.Close()
		}
	}
}

func printEntries(entries []anyshare.Entry, asJSON bool) error {
	if asJSON {
		e := json.NewEncoder(os.Stdout)
		e.SetIndent("", "  ")
		return e.Encode(entries)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tBYTES\tID")
	for _, e := range entries {
		typ := e.Type
		if typ == "" {
			if e.Dir {
				typ = "folder"
			} else {
				typ = "file"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", e.Name, typ, e.Size, e.ID)
	}
	return w.Flush()
}

func recovery(args []string) error {
	set := flag.NewFlagSet("recover", flag.ContinueOnError)
	cache := set.String("cache-dir", defaultCache(), "private write-cache directory")
	asJSON := set.Bool("json", false, "output recovery records as JSON")
	if err := set.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	entries, err := vfs.RecoveryFiles(*cache)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(entries)
	}
	for _, e := range entries {
		fmt.Printf("%s\n  remote: %s\n  revision: %s\n", e.Data, e.Entry.ID, e.Entry.Rev)
	}
	return nil
}
