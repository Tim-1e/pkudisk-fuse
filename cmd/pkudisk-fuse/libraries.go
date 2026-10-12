package main

import (
	"context"
	"fmt"
	"io"

	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
)

type libraryStats interface {
	LibraryQuota(context.Context, string) (anyshare.Quota, error)
	Writable(context.Context, string) (bool, error)
}

func printLibraries(ctx context.Context, client libraryStats, entries []anyshare.Entry, sizes sizeFormat, output, status io.Writer) error {
	rows := make([][]string, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		storage, access := "unavailable", "?"
		writable, err := client.Writable(ctx, entry.ID)
		if err != nil {
			fmt.Fprintf(status, "Library %q permissions: %v\n", tuiText(entry.Name), err)
		} else if writable {
			access = "RW"
		} else {
			access = "RO"
		}
		quota, err := client.LibraryQuota(ctx, entry.ID)
		if err != nil {
			fmt.Fprintf(status, "Library %q usage: %v\n", tuiText(entry.Name), err)
		} else {
			storage = sizes.value(quota.Used)
			if access == "RW" {
				storage += " / " + sizes.value(quota.Allocated)
				if quota.Allocated > 0 {
					storage += fmt.Sprintf(" (%.1f%%)", 100*float64(quota.Used)/float64(quota.Allocated))
				}
			}
		}
		rows = append(rows, []string{entry.Name, entry.Type, storage, access, entry.ID})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return printTable(output, []string{"NAME", "TYPE", "USED", "ACCESS", "ID"}, rows, 2)
}
