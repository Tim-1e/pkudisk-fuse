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
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"pkudisk-fuse/internal/accounts"
	"pkudisk-fuse/internal/auth"
)

type authFlags struct {
	server, account, username, passwordFile, credentialsFile string
	stdin, noSave                                            bool
	saveName                                                 string
	forceLogin, makeDefault, replace                         bool
	announce                                                 bool
}

func (f *authFlags) addCredentials(set *flag.FlagSet) {
	set.StringVar(&f.server, "server", "", "AnyShare server URL (saved account's server or "+auth.DefaultServer+")")
	set.StringVar(&f.username, "username", "", "login username (prompted if omitted)")
	set.StringVar(&f.passwordFile, "password-file", "", "read password from an owner-only file")
	set.StringVar(&f.credentialsFile, "credentials-file", "", "read campus username/password from an owner-only JSON file")
	set.BoolVar(&f.stdin, "password-stdin", false, "read password from standard input")
}

func (f *authFlags) add(set *flag.FlagSet) {
	f.addCredentials(set)
	set.StringVar(&f.account, "account", "", "use this saved account instead of the default")
	set.BoolVar(&f.noSave, "no-save", false, "use a one-off login without reading or saving accounts")
}

func privateFile(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
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
		clear(b)
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

func readCredentials(opts authFlags) (auth.Credentials, error) {
	var c auth.Credentials
	if opts.credentialsFile != "" {
		b, err := privateFile(opts.credentialsFile)
		if err != nil {
			return c, err
		}
		defer clear(b)
		var v struct {
			Username string `json:"username"`
			Password string `json:"password"`
			OTP      string `json:"otp"`
			SMS      string `json:"sms"`
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return c, errors.New("invalid credentials JSON")
		}
		return auth.Credentials{Username: v.Username, Password: []byte(v.Password), OTP: v.OTP, SMS: v.SMS}, nil
	}
	c.Username = opts.username
	if c.Username == "" {
		b, err := ttyInput("Username: ", false)
		if err != nil {
			return c, err
		}
		c.Username = string(b)
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
		clear(c.Password)
	}
	return c, err
}

func verificationInput(ctx context.Context, challenge auth.Challenge) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(challenge.Image) > 0 {
		file, err := os.CreateTemp("", "pkudisk-captcha-*.png")
		if err != nil {
			return "", err
		}
		defer os.Remove(file.Name())
		_, err = file.Write(challenge.Image)
		closeErr := file.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
		fmt.Fprintln(os.Stderr, "CAPTCHA image:", file.Name())
	}
	b, err := ttyInput(challenge.Kind+": ", true)
	defer clear(b)
	return string(b), err
}

func authenticate(ctx context.Context, opts authFlags) (*auth.Session, string, string, error) {
	return authenticateWith(ctx, opts, auth.New)
}

func authenticateWith(ctx context.Context, opts authFlags, newSession func(auth.Options) (*auth.Session, error)) (*auth.Session, string, string, error) {
	explicit := opts.username != "" || opts.passwordFile != "" || opts.credentialsFile != "" || opts.stdin
	if opts.credentialsFile != "" && (opts.username != "" || opts.passwordFile != "" || opts.stdin) {
		return nil, "", "", errors.New("--credentials-file cannot be combined with other credential options")
	}
	if opts.passwordFile != "" && opts.stdin {
		return nil, "", "", errors.New("choose either --password-file or --password-stdin")
	}
	if opts.account != "" && (explicit || opts.noSave) {
		return nil, "", "", errors.New("--account cannot be combined with explicit credentials or --no-save")
	}
	var store *accounts.Store
	var config accounts.Config
	if !opts.noSave {
		var err error
		store, err = accounts.Default()
		if err != nil {
			return nil, "", "", err
		}
		config, err = store.Load()
		if err != nil {
			return nil, "", "", fmt.Errorf("load saved accounts: %w", err)
		}
	}
	var saved *accounts.Account
	if !explicit && !opts.forceLogin && !opts.noSave {
		name := opts.account
		if name == "" {
			name = config.Default
		}
		if name != "" {
			a, ok := config.Find(name)
			if !ok {
				return nil, "", "", fmt.Errorf("%w: %s", accounts.ErrNotFound, name)
			}
			if opts.server != "" && strings.TrimRight(opts.server, "/") != strings.TrimRight(a.Server, "/") {
				return nil, "", "", errors.New("--server differs from the saved account's server; provide explicit credentials for another server")
			}
			saved, opts.server = &a, a.Server
		}
	}
	if opts.server == "" {
		opts.server = auth.DefaultServer
	}
	opts.server = strings.TrimRight(opts.server, "/")
	var c auth.Credentials
	var err error
	if saved != nil {
		c = auth.Credentials{Username: saved.Username, Password: []byte(saved.Password)}
	} else {
		c, err = readCredentials(opts)
		if err != nil {
			return nil, "", "", err
		}
	}
	defer clear(c.Password)
	if c.Username == "" || len(c.Password) == 0 || len(c.Password) > 8192 {
		return nil, "", "", errors.New("username and a password of at most 8192 bytes are required")
	}
	options := auth.Options{Server: opts.server, Challenge: verificationInput}
	// Any explicit login without a selected saved account creates or updates a
	// persistent account. With no existing default, the first login becomes it.
	saveNew := !opts.noSave && saved == nil
	name := opts.saveName
	if name == "" {
		name = c.Username
	}
	accountLabel := name
	if saved != nil {
		accountLabel = saved.Name
	}
	if !opts.noSave {
		persistedName, persistedServer, persistedUsername := name, opts.server, c.Username
		if saved != nil {
			persistedName, persistedServer, persistedUsername = saved.Name, saved.Server, saved.Username
		}
		options.RefreshLock = store.LockSession
		options.LoadState = func() (auth.State, error) {
			latest, err := store.Load()
			if err != nil {
				return auth.State{}, err
			}
			account, ok := latest.Find(persistedName)
			if !ok || account.Username != persistedUsername || strings.TrimRight(account.Server, "/") != strings.TrimRight(persistedServer, "/") {
				return auth.State{}, fmt.Errorf("saved account %q changed while the session was refreshing", persistedName)
			}
			return account.Session, nil
		}
	}
	if saveNew {
		if !accounts.ValidName(name) {
			return nil, "", "", errors.New("invalid account name")
		}
		replaceAccount := opts.replace
		if existing, exists := config.Find(name); exists {
			if existing.Server != opts.server {
				return nil, "", "", fmt.Errorf("account %q already belongs to %s; choose another account name", name, existing.Server)
			}
			// Explicit credentials on a normal command are a deliberate
			// re-login, so refresh the matching saved account. `account add`
			// remains conservative and requires --replace.
			if opts.forceLogin && !opts.replace {
				return nil, "", "", accounts.ErrExists
			}
			replaceAccount = true
		}
		record := accounts.Account{Name: name, Username: c.Username, Password: string(c.Password), Server: opts.server}
		created := false
		options.OnState = func(state auth.State) error {
			if created {
				return store.UpdateState(name, record.Server, record.Username, state)
			}
			record.Session = state
			makeDefault := opts.makeDefault || config.Default == ""
			if err := store.Put(record, makeDefault, replaceAccount); err != nil {
				return err
			}
			created = true
			record.Password = ""
			return nil
		}
	} else if saved != nil {
		name, server, username := saved.Name, saved.Server, saved.Username
		options.OnState = func(state auth.State) error { return store.UpdateState(name, server, username, state) }
	}
	if opts.announce {
		if accountLabel == c.Username {
			fmt.Fprintf(os.Stderr, "Using account %q (%s)\n", accountLabel, opts.server)
		} else {
			fmt.Fprintf(os.Stderr, "Using account %q (username %q, %s)\n", accountLabel, c.Username, opts.server)
		}
	}
	s, err := newSession(options)
	if err != nil {
		return nil, "", "", err
	}
	if saved != nil && saved.Session.RefreshToken != "" {
		err = s.Restore(ctx, saved.Session)
		if err == nil {
			return s, opts.server, accountLabel, nil
		}
		if !errors.Is(err, auth.ErrSessionExpired) {
			s.Close()
			return nil, "", "", fmt.Errorf("restore account %q: %w", saved.Name, err)
		}
		s.Close()
	}
	if err = s.Login(ctx, c); err != nil {
		s.Close()
		return nil, "", "", err
	}
	if saveNew {
		suffix := ""
		if opts.makeDefault || config.Default == "" {
			suffix = " (default)"
		}
		fmt.Fprintf(os.Stderr, "Saved account %q%s.\n", name, suffix)
	}
	return s, opts.server, accountLabel, nil
}
