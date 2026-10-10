package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"pkudisk-fuse/internal/anyshare"
	"pkudisk-fuse/internal/auth"
	"pkudisk-fuse/internal/vfs"
)

var version = "dev"

type authFlags struct {
	server, username, passwordFile, credentialsFile string
	stdin                                           bool
}

func (f *authFlags) add(set *flag.FlagSet) {
	set.StringVar(&f.server, "server", "https://disk.pku.edu.cn", "AnyShare server URL")
	set.StringVar(&f.username, "username", "", "login username (prompted if omitted)")
	set.StringVar(&f.passwordFile, "password-file", "", "read password from an owner-only file")
	set.StringVar(&f.credentialsFile, "credentials-file", "", "read campus username/password from an owner-only JSON file")
	set.BoolVar(&f.stdin, "password-stdin", false, "read password from standard input")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pkudisk-fuse:", err)
		os.Exit(1)
	}
}
func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  pkudisk-fuse mount [options] MOUNTPOINT
  pkudisk-fuse roots [authentication options] [--json]
  pkudisk-fuse ls [authentication options] [--json] PATH-OR-GNS-ID
  pkudisk-fuse recover [--cache-dir PATH] [--json]
  pkudisk-fuse version

Mount requires username and password. Use --username and a hidden password
prompt, --password-file, --password-stdin, or --credentials-file.
--root '/LIBRARY/FOLDER' or --root 'gns://...' selects a library/folder;
omitting it mounts all accessible libraries. Run 'mount --help' for options.`)
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
	case "mount", "roots", "ls":
	default:
		usage()
		return errors.New("unknown command")
	}
	command := args[0]
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	var credentials authFlags
	credentials.add(set)
	var root, cache string
	var readOnly, debug, jsonOutput bool
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
	} else {
		set.BoolVar(&jsonOutput, "json", false, "output entries as JSON")
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
	if command == "ls" && set.NArg() != 1 {
		return errors.New("ls requires one library/folder path or gns:// ID")
	}
	if command == "roots" && set.NArg() != 0 {
		return errors.New("roots takes no positional arguments")
	}
	if command == "mount" && (cacheLimit < 0 || partSize < 5<<20 || ttl < 0 || timeout <= 0 || refreshInterval < 0) {
		return errors.New("invalid cache, part-size, or timeout option")
	}
	ctx := context.Background()
	session, err := authenticate(ctx, credentials)
	if err != nil {
		return err
	}
	defer session.Close()
	client, err := anyshare.New(credentials.server, session)
	if err != nil {
		return err
	}
	if command == "roots" {
		entries, err := client.Roots(ctx)
		if err != nil {
			return err
		}
		return printEntries(entries, jsonOutput)
	}
	if command == "ls" {
		var entries []anyshare.Entry
		if set.Arg(0) == "/" {
			entries, err = client.Roots(ctx)
		} else {
			var e anyshare.Entry
			e, err = client.Resolve(ctx, set.Arg(0))
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

func privateFile(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != uint32(os.Getuid()) {
		return nil, errors.New("credential file must be a regular owner-only file (chmod 600)")
	}
	b, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil {
		return nil, err
	}
	if len(b) > 8192 {
		return nil, errors.New("credential file is too large")
	}
	return b, nil
}

func passwordBytes(b []byte) []byte {
	return bytes.TrimSuffix(bytes.TrimSuffix(b, []byte("\n")), []byte("\r"))
}

func ttyInput(label string, hidden bool) ([]byte, error) {
	file, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("interactive terminal unavailable; provide --username and --password-file")
	}
	defer file.Close()
	fmt.Fprint(file, label)
	if hidden {
		b, err := term.ReadPassword(int(file.Fd()))
		fmt.Fprintln(file)
		return b, err
	}
	line, err := bufio.NewReader(file).ReadString('\n')
	return []byte(strings.TrimSpace(line)), err
}

func authenticate(ctx context.Context, opts authFlags) (*auth.Session, error) {
	var c auth.Credentials
	if opts.credentialsFile != "" {
		if opts.username != "" || opts.passwordFile != "" || opts.stdin {
			return nil, errors.New("--credentials-file cannot be combined with other credential options")
		}
		b, err := privateFile(opts.credentialsFile)
		if err != nil {
			return nil, err
		}
		defer clear(b)
		var v struct {
			Username string `json:"username"`
			Password string `json:"password"`
			OTP      string `json:"otp"`
			SMS      string `json:"sms"`
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return nil, errors.New("invalid credentials JSON")
		}
		c = auth.Credentials{Username: v.Username, Password: []byte(v.Password), OTP: v.OTP, SMS: v.SMS}
	} else {
		c.Username = opts.username
		if c.Username == "" {
			b, err := ttyInput("Username: ", false)
			if err != nil {
				return nil, err
			}
			c.Username = string(b)
		}
		if opts.passwordFile != "" && opts.stdin {
			return nil, errors.New("choose either --password-file or --password-stdin")
		}
		var err error
		switch {
		case opts.passwordFile != "":
			c.Password, err = privateFile(opts.passwordFile)
			c.Password = passwordBytes(c.Password)
		case opts.stdin:
			c.Password, err = io.ReadAll(io.LimitReader(os.Stdin, 8193))
			c.Password = passwordBytes(c.Password)
		default:
			c.Password, err = ttyInput("Password: ", true)
		}
		if err != nil {
			return nil, err
		}
	}
	defer clear(c.Password)
	if len(c.Password) > 8192 {
		return nil, errors.New("password is too long")
	}
	s, err := auth.New(auth.Options{Server: opts.server, Challenge: func(ctx context.Context, challenge auth.Challenge) (string, error) {
		if len(challenge.Image) > 0 {
			file, err := os.CreateTemp("", "pkudisk-captcha-*.png")
			if err != nil {
				return "", err
			}
			path := file.Name()
			defer os.Remove(path)
			_, err = file.Write(challenge.Image)
			file.Close()
			if err != nil {
				return "", err
			}
			fmt.Fprintln(os.Stderr, "CAPTCHA image:", path)
		}
		b, err := ttyInput(challenge.Kind+": ", true)
		if err != nil {
			return "", err
		}
		defer clear(b)
		return string(b), nil
	}})
	if err != nil {
		return nil, err
	}
	if err = s.Login(ctx, c); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
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
