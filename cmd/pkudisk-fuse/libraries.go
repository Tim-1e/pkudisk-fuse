package main

import (
	"context"
	"fmt"
	"io"

	"github.com/dustin/go-humanize"
	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
)

type libraryStats interface {
	LibraryQuota(context.Context, string) (anyshare.Quota, error)
	Writable(context.Context, string) (bool, error)
}

func printLibraries(ctx context.Context, client libraryStats, entries []anyshare.Entry, output, status io.Writer) error {
	rows := make([][]string, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		storage, access := "unavailable", "?"
		quota, err := client.LibraryQuota(ctx, entry.ID)
		if err != nil {
			fmt.Fprintf(status, "Library %q usage: %v\n", tuiText(entry.Name), err)
		} else {
			storage = fmt.Sprintf("%s / %s", humanize.IBytes(uint64(quota.Used)), humanize.IBytes(uint64(quota.Allocated)))
			if quota.Allocated > 0 {
				storage += fmt.Sprintf(" (%.1f%%)", 100*float64(quota.Used)/float64(quota.Allocated))
			}
		}
		writable, err := client.Writable(ctx, entry.ID)
		if err != nil {
			fmt.Fprintf(status, "Library %q permissions: %v\n", tuiText(entry.Name), err)
		} else if writable {
			access = "RW"
		} else {
			access = "RO"
		}
		rows = append(rows, []string{entry.Name, entry.Type, storage, access, entry.ID})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return printTable(output, []string{"NAME", "TYPE", "USED / QUOTA", "ACCESS", "ID"}, rows, 2)
}
