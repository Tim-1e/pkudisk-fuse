package main

import (
	"context"
	"fmt"
	"io"

	"pkudisk-fuse/internal/anyshare"
	"pkudisk-fuse/internal/transfer"
)

func runTransfer(ctx context.Context, client *anyshare.Client, direction, source, destination string, overwrite bool, out, errOut io.Writer) error {
	service := transfer.Service{Backend: client}
	progress := newCLIProgress(direction, errOut)
	var result transfer.Result
	var err error
	if direction == "upload" {
		result, err = service.Upload(ctx, source, destination, overwrite, progress.Update)
	} else {
		result, err = service.Download(ctx, source, destination, overwrite, progress.Update)
	}
	progress.Finish(err)
	if err != nil {
		return fmt.Errorf("%s %q: %w", direction, source, err)
	}
	if direction == "upload" {
		fmt.Fprintf(out, "Uploaded %s -> %s (%d bytes)\n", result.Local, result.Remote, result.Entry.Size)
	} else {
		fmt.Fprintf(out, "Downloaded %s -> %s (%d bytes)\n", result.Remote, result.Local, result.Entry.Size)
	}
	return nil
}
