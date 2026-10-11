package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
)

func newUtilCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "util",
		Short: "Use small server-side NetDisk utilities",
		Long: `Run focused server-side operations without mounting or copying bytes
through the local machine. These operations are intentionally separate from
file transfers: the server's permissions, policies and storage layout decide
whether they are available.

Commands:
  quota   show the account's server allocation and writable remaining bytes
  search  search remote filenames, optionally inside a folder
  copy    copy a remote file or folder into another remote folder

Use 'pkudisk-fuse util COMMAND --help' for request scope, examples and limits.`,
		Example: "  pkudisk-fuse util quota\n  pkudisk-fuse util search report\n  pkudisk-fuse util copy '/Library/report.pdf' '/Library/Archive'",
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newUtilQuotaCommand(), newUtilSearchCommand(), newUtilCopyCommand())
	return cmd
}

type utilAuth struct {
	credentials authFlags
	timeout     time.Duration
}

func (o *utilAuth) add(cmd *cobra.Command) {
	o.timeout = 10 * time.Minute
	o.credentials.add(cmd.Flags())
	cmd.Flags().DurationVar(&o.timeout, "io-timeout", o.timeout, "timeout for the server operation; accepts 30s, 10m, 1h")
}

type utilCloser interface{ Close() }

func (o *utilAuth) open(cmd *cobra.Command) (*anyshare.Client, utilCloser, error) {
	if err := o.credentials.validate(); err != nil {
		return nil, nil, usageFailure(cmd, err.Error())
	}
	if o.timeout <= 0 {
		return nil, nil, usageFailure(cmd, "--io-timeout must be positive")
	}
	o.credentials.input, o.credentials.output = cmd.InOrStdin(), cmd.ErrOrStderr()
	o.credentials.announce = true
	session, server, _, err := authenticate(cmd.Context(), o.credentials)
	if err != nil {
		return nil, nil, err
	}
	client, err := anyshare.New(server, session)
	if err != nil {
		session.Close()
		return nil, nil, err
	}
	return client, session, nil
}

func newUtilQuotaCommand() *cobra.Command {
	var auth utilAuth
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Show server allocation and writable remaining capacity",
		Long: `Read the authenticated account's server quota. Available is
allocated minus used, clamped at zero; it excludes local cache free space.
This is the same value exposed as free space by a FUSE mount. The endpoint is
read-only and does not enumerate files.`,
		Example: "  pkudisk-fuse util quota\n  pkudisk-fuse util quota --account work",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, closer, err := auth.open(cmd)
			if err != nil {
				return err
			}
			defer closer.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), auth.timeout)
			defer cancel()
			quota, err := client.Quota(ctx)
			if err != nil {
				return fmt.Errorf("read quota: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Allocated: %s (%d bytes)\n", humanize.IBytes(uint64(quota.Allocated)), quota.Allocated)
			fmt.Fprintf(out, "Used:      %s (%d bytes)\n", humanize.IBytes(uint64(quota.Used)), quota.Used)
			fmt.Fprintf(out, "Available: %s (%d bytes)\n", humanize.IBytes(uint64(quota.Available())), quota.Available())
			return nil
		},
	}
	auth.add(cmd)
	return cmd
}

func newUtilSearchCommand() *cobra.Command {
	var auth utilAuth
	var scope string
	var limit int
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:     "search KEYWORD",
		Short:   "Search remote filenames",
		Args:    cobra.ExactArgs(1),
		Example: "  pkudisk-fuse util search report\n  pkudisk-fuse util search report --in '/Library/Reports' --limit 100\n  pkudisk-fuse util search report --json",
		Long: `Search file and folder names through AnyShare's server index.
The default searches all accessible libraries. --in resolves a remote folder
first and restricts the server query to that folder's descendants. Results are
one bounded page; --limit accepts 1 through 1000. Content and paragraph search
are not requested. A search never changes remote files.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, closer, err := auth.open(cmd)
			if err != nil {
				return err
			}
			defer closer.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), auth.timeout)
			defer cancel()
			selector := ""
			if scope != "" {
				entry, err := client.Resolve(ctx, scope)
				if err != nil {
					return fmt.Errorf("resolve search scope: %w", err)
				}
				if !entry.Dir {
					return errors.New("search scope must be a remote folder")
				}
				selector = entry.ID
			}
			result, err := client.Search(ctx, args[0], selector, limit)
			if err != nil {
				return fmt.Errorf("search remote names: %w", err)
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Hits  int                    `json:"hits"`
					Next  int                    `json:"next"`
					Files []anyshare.SearchEntry `json:"files"`
				}{result.Hits, result.Next, result.Files})
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSIZE\tPATH\tID")
			for _, file := range result.Files {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", file.Name, humanize.IBytes(uint64(file.Size)), file.Path, file.ID)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Matches: %d; returned: %d; next: %d\n", result.Hits, len(result.Files), result.Next)
			return nil
		},
	}
	auth.add(cmd)
	cmd.Flags().StringVar(&scope, "in", "", "restrict the search to a remote folder path or gns:// ID")
	cmd.Flags().IntVar(&limit, "limit", 25, "maximum results to request (1-1000)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "write matches and pagination fields as JSON")
	return cmd
}

func newUtilCopyCommand() *cobra.Command {
	var auth utilAuth
	cmd := &cobra.Command{
		Use:   "copy REMOTE-SOURCE REMOTE-DIRECTORY",
		Short: "Copy a remote file or folder on the server",
		Args:  cobra.ExactArgs(2),
		Long: `Copy a remote file or directory directly on AnyShare. The destination
must be an existing remote folder; the source keeps its current name. File
copies return after the server publishes the new object. Directory copies may
run asynchronously and are polled until completion. No file bytes pass through
this client. Existing-name behavior follows the official client's server
policy; inspect the destination with ls after completion.`,
		Example: "  pkudisk-fuse util copy '/Library/report.pdf' '/Library/Archive'\n  pkudisk-fuse util copy '/Library/Photos' '/Library/Backup'",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, closer, err := auth.open(cmd)
			if err != nil {
				return err
			}
			defer closer.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), auth.timeout)
			defer cancel()
			source, err := client.Resolve(ctx, args[0])
			if err != nil {
				return fmt.Errorf("resolve copy source: %w", err)
			}
			destination, err := client.Resolve(ctx, args[1])
			if err != nil {
				return fmt.Errorf("resolve copy destination: %w", err)
			}
			if !destination.Dir {
				return errors.New("copy destination must be a remote folder")
			}
			result, err := client.Copy(ctx, source, destination, func(progress anyshare.CopyProgress) {
				if source.Dir && progress.Status != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "Copying %s: %d%% (%s)\n", source.Name, progress.Percent, progress.Status)
				}
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Copied %s -> %s/%s\n", source.ID, destination.ID, result.Name)
			return nil
		},
	}
	auth.add(cmd)
	return cmd
}
