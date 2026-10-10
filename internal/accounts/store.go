// Package accounts stores login credentials and renewable sessions in a private,
// atomically replaced file. A file lock protects updates from other processes.
package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"pkudisk-fuse/internal/auth"
)

var (
	ErrNotFound = errors.New("saved account not found")
	ErrExists   = errors.New("saved account already exists; use --replace to update it")
)

type Account struct {
	Name     string     `json:"name"`
	Server   string     `json:"server"`
	Username string     `json:"username"`
	Password string     `json:"password"`
	Session  auth.State `json:"session"`
}

type Config struct {
	Version  int       `json:"version"`
	Default  string    `json:"default"`
	Accounts []Account `json:"accounts"`
}

func (c Config) Find(name string) (Account, bool) {
	for _, account := range c.Accounts {
		if account.Name == name {
			return account, true
		}
	}
	return Account{}, false
}

type Store struct{ Path string }

func Default() (*Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &Store{Path: filepath.Join(dir, "pkudisk-fuse", "accounts.json")}, nil
}

func ValidName(name string) bool {
	return name != "" && len(name) <= 128 && utf8.ValidString(name) && strings.TrimSpace(name) == name &&
		!strings.ContainsAny(name, "/\\") && strings.IndexFunc(name, unicode.IsControl) == -1
}

func validate(c Config) error {
	if c.Version != 1 {
		return errors.New("unsupported account store version")
	}
	names := make(map[string]bool)
	for _, a := range c.Accounts {
		u, err := url.Parse(a.Server)
		if !ValidName(a.Name) || names[a.Name] || a.Username == "" || strings.IndexFunc(a.Username, unicode.IsControl) >= 0 ||
			len(a.Password) == 0 || len(a.Password) > 8192 || err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))) {
			return errors.New("invalid saved account")
		}
		names[a.Name] = true
	}
	if (len(c.Accounts) == 0 && c.Default != "") || (len(c.Accounts) > 0 && !names[c.Default]) {
		return errors.New("invalid default account")
	}
	return nil
}

func private(info os.FileInfo, directory bool) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 ||
		(directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return errors.New("account storage must be owned by the current user with directory mode 0700 and file mode 0600")
	}
	return nil
}

func openPrivate(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err == nil {
		err = private(info, false)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (s *Store) lock(write bool) (*os.File, error) {
	dir := filepath.Dir(s.Path)
	if write {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if err = private(info, true); err != nil {
		return nil, err
	}
	f, err := openPrivate(s.Path+".lock", unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return nil, err
	}
	mode := unix.LOCK_SH
	if write {
		mode = unix.LOCK_EX
	}
	if err = unix.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// LockSession serializes refresh-token rotation across processes. The lock is
// context-aware because a crashed or stuck peer must not hang an API request
// forever. It uses a separate lock file so the holder can still update the
// account JSON from its authentication callback.
func (s *Store) LockSession(ctx context.Context) (io.Closer, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if err = private(info, true); err != nil {
		return nil, err
	}
	f, err := openPrivate(s.Path+".session.lock", unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) read() (Config, error) {
	empty := Config{Version: 1, Accounts: []Account{}}
	f, err := openPrivate(s.Path, unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	defer clear(b)
	if err != nil || len(b) > 1<<20 {
		return empty, errors.New("cannot read account store or account store exceeds 1 MiB")
	}
	var c Config
	if err = json.Unmarshal(b, &c); err != nil {
		return empty, errors.New("invalid account store JSON")
	}
	if err = validate(c); err != nil {
		return empty, err
	}
	return c, nil
}

func (s *Store) Load() (Config, error) {
	f, err := s.lock(false)
	if errors.Is(err, os.ErrNotExist) {
		return Config{Version: 1, Accounts: []Account{}}, nil
	}
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	return s.read()
}

func (s *Store) update(change func(*Config) error) error {
	lock, err := s.lock(true)
	if err != nil {
		return err
	}
	defer lock.Close()
	c, err := s.read()
	if err != nil {
		return err
	}
	if err = change(&c); err != nil {
		return err
	}
	if err = validate(c); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return errors.New("cannot encode account store")
	}
	defer clear(b)
	if len(b) > 1<<20 {
		return errors.New("account store exceeds 1 MiB")
	}
	f, err := os.CreateTemp(filepath.Dir(s.Path), ".accounts-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.Path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.Path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Store) Put(account Account, makeDefault, replace bool) error {
	return s.update(func(c *Config) error {
		found := false
		for i, a := range c.Accounts {
			if a.Name == account.Name {
				if !replace {
					return ErrExists
				}
				c.Accounts[i], found = account, true
				break
			}
		}
		if !found {
			c.Accounts = append(c.Accounts, account)
		}
		if makeDefault || c.Default == "" {
			c.Default = account.Name
		}
		return nil
	})
}

func (s *Store) SetDefault(name string) error {
	return s.update(func(c *Config) error {
		if _, ok := c.Find(name); !ok {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		c.Default = name
		return nil
	})
}

func (s *Store) Remove(name string) error {
	return s.update(func(c *Config) error {
		for i, a := range c.Accounts {
			if a.Name != name {
				continue
			}
			c.Accounts = append(c.Accounts[:i], c.Accounts[i+1:]...)
			if c.Default == name {
				c.Default = ""
				if len(c.Accounts) > 0 {
					c.Default = c.Accounts[0].Name
				}
			}
			return nil
		}
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	})
}

// UpdateState changes only the renewable session. A mount cannot resurrect an
// account removed by another command or overwrite that account's credentials.
func (s *Store) UpdateState(name, server, username string, state auth.State) error {
	return s.update(func(c *Config) error {
		found := false
		for i, a := range c.Accounts {
			if a.Name == name && a.Server == server && a.Username == username {
				c.Accounts[i].Session = state
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return nil
	})
}
