package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/thezzisu/pkudisk-fuse/internal/accounts"
	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
	"github.com/thezzisu/pkudisk-fuse/internal/auth"
	"github.com/thezzisu/pkudisk-fuse/internal/diagnostic"
	"github.com/thezzisu/pkudisk-fuse/internal/vfs"
)

type accountSession struct {
	name    string
	session *auth.Session
}

type accountSource struct {
	backend  vfs.Backend
	root     *anyshare.Entry
	sessions []accountSession
}

type accountOptions struct {
	allAccounts bool
	root        string
	partSize    int64
	timeout     time.Duration
	trace       *diagnostic.Logger
}

// Mounts and browsers share account selection, root validation and session lifetime.
func prepareAccounts(ctx context.Context, credentials authFlags, opts accountOptions) (_ *accountSource, result error) {
	source := new(accountSource)
	defer func() {
		if result != nil {
			source.Close()
		}
	}()
	names := []string{credentials.account}
	if opts.allAccounts {
		store, err := accounts.Default()
		if err != nil {
			return nil, err
		}
		config, err := store.Load()
		if err != nil {
			return nil, fmt.Errorf("load saved accounts: %w", err)
		}
		if len(config.Accounts) == 0 {
			return nil, errors.New("no saved accounts; run 'pkudisk-fuse account add' first")
		}
		names = nil
		for _, account := range config.Accounts {
			names = append(names, account.Name)
		}
	}
	var mounted []vfs.MountAccount
	for _, name := range names {
		credentials.account = name
		session, server, label, err := authenticate(ctx, credentials)
		if err != nil {
			if name == "" {
				return nil, err
			}
			return nil, fmt.Errorf("prepare account %q: %w", name, err)
		}
		source.sessions = append(source.sessions, accountSession{label, session})
		client, err := anyshare.New(server, session)
		if err != nil {
			return nil, err
		}
		if opts.partSize > 0 {
			client.PartSize = opts.partSize
		}
		client.SetTrace(opts.trace)
		if err := selectMountRoot(ctx, source, client, opts); err != nil {
			return nil, fmt.Errorf("prepare account %q: %w", label, err)
		}
		mounted = append(mounted, vfs.MountAccount{Name: label, Server: server, Backend: client})
		source.backend = client
	}
	if opts.allAccounts {
		backend, err := vfs.NewAccountBackend(mounted)
		if err != nil {
			return nil, err
		}
		source.backend = backend
	}
	return source, nil
}

func selectMountRoot(ctx context.Context, source *accountSource, client *anyshare.Client, opts accountOptions) error {
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	if opts.root == "" || opts.root == "/" {
		_, err := client.Roots(ctx)
		return err
	}
	entry, err := client.Resolve(ctx, opts.root)
	if err != nil {
		return fmt.Errorf("select mount root: %w", err)
	}
	if !entry.Dir {
		return errors.New("mount root must be a remote directory")
	}
	source.root = &entry
	return nil
}

func (s *accountSource) Close() {
	for _, account := range s.sessions {
		account.session.Close()
	}
}

func (s *accountSource) maintain(ctx context.Context, interval time.Duration, output io.Writer) (<-chan error, func()) {
	ctx, cancel := context.WithCancel(ctx)
	failed := make(chan error, len(s.sessions))
	var workers sync.WaitGroup
	var outputMu sync.Mutex
	for _, account := range s.sessions {
		workers.Go(func() {
			account.session.KeepAlive(ctx, interval, func(err error) {
				if errors.Is(err, auth.ErrSessionExpired) {
					failed <- fmt.Errorf("account %q: %w", account.name, err)
					return
				}
				outputMu.Lock()
				defer outputMu.Unlock()
				if err == nil {
					fmt.Fprintf(output, "Account %q: authentication session renewed.\n", account.name)
				} else {
					fmt.Fprintf(output, "Account %q: session renewal will retry: %v\n", account.name, err)
				}
			})
		})
	}
	return failed, func() { cancel(); workers.Wait() }
}
