package anyshare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

type ArchiveProgress struct {
	Preparing   bool
	Percent     int
	Done, Total int64 // Total is -1 when the response has no Content-Length.
}

// DownloadArchive asks the server to package a directory. Polling repeats the
// same request, as in the official client; it is not a transport-error retry.
func (c *Client) DownloadArchive(ctx context.Context, entry Entry, dest io.Writer, observe func(ArchiveProgress)) (int64, error) {
	if !entry.Dir || !ValidName(entry.Name) {
		return 0, errors.New("archive download requires a remote directory")
	}
	name := entry.Name + ".zip"
	if !ValidName(name) {
		name = "archive.zip"
	}
	body := map[string]any{"name": name, "doc": []map[string]string{{"id": objectID(entry.ID)}}}
	for {
		var out struct {
			Progress *int   `json:"progress"`
			Address  string `json:"package_address"`
			Items    []struct {
				Status string `json:"status"`
				Error  struct {
					Code int `json:"code"`
				} `json:"error"`
			} `json:"items"`
		}
		if err := c.post(ctx, "/open-doc/v1/file-download", body, &out); err != nil {
			return 0, fmt.Errorf("prepare remote ZIP: %w", archiveError(err))
		}
		if out.Progress == nil || *out.Progress < 0 || *out.Progress > 100 {
			return 0, errors.New("archive response omitted valid progress")
		}
		if observe != nil {
			observe(ArchiveProgress{Preparing: true, Percent: *out.Progress})
		}
		if *out.Progress < 100 {
			if err := pollDelay(ctx); err != nil {
				return 0, err
			}
			continue
		}
		for _, item := range out.Items {
			if item.Status != "completed" || item.Error.Code != 0 {
				return 0, fmt.Errorf("remote ZIP is incomplete (item status %q, code %d); download the folder without archive flags to inspect individual failures", item.Status, item.Error.Code)
			}
		}
		address, err := c.archiveAddress(out.Address)
		if err != nil {
			return 0, err
		}
		resp, err := c.signed(ctx, signedRequest{"GET", address}, nil, 0, nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return 0, errors.New("archive download did not return a complete response")
		}
		writer := &archiveWriter{dest: dest, total: resp.ContentLength, observe: observe}
		writer.report()
		n, err := io.Copy(writer, resp.Body)
		if err == nil && resp.ContentLength >= 0 && n != resp.ContentLength {
			err = errors.New("archive download size differs from Content-Length")
		}
		return n, err
	}
}

// Package URLs use the account token, so only send it to this server's origin.
// The live server spells the default HTTPS port explicitly as :443.
func (c *Client) archiveAddress(address string) (string, error) {
	base, _ := url.Parse(c.base)
	u, err := url.Parse(address)
	if err != nil || address == "" {
		return "", errors.New("archive response omitted a valid package address")
	}
	u = base.ResolveReference(u)
	port := func(v *url.URL) string {
		if v.Port() != "" {
			return v.Port()
		}
		if v.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	if u.User != nil || u.Fragment != "" || u.Scheme != base.Scheme || !strings.EqualFold(u.Hostname(), base.Hostname()) || port(u) != port(base) {
		return "", errors.New("archive package address is outside the server origin")
	}
	q := u.Query()
	q.Set("token", c.tokens.AccessToken())
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Extract creates a new remote folder. Stopping polling does not cancel the
// server job: the official protocol exposes no cancellation endpoint.
func (c *Client) Extract(ctx context.Context, archive Entry, parent, name string, observe func(int)) (Entry, error) {
	if archive.Dir || !ValidName(name) || parent == "" {
		return Entry{}, errors.New("extraction requires an archive file and a destination folder name")
	}
	body := map[string]any{"doc_id": objectID(archive.ID), "target_path": parent, "name": name, "ondup": 1}
	for {
		var out struct {
			Progress *int   `json:"progress"`
			Status   string `json:"status"`
			Path     string `json:"unarchive_path"`
		}
		if err := c.post(ctx, "/open-doc/v1/file-unarchiver", body, &out); err != nil {
			return Entry{}, fmt.Errorf("remote extraction: %w", archiveError(err))
		}
		if out.Progress == nil || *out.Progress < 0 || *out.Progress > 100 || (out.Status != "extracting" && out.Status != "complete") {
			return Entry{}, errors.New("server returned an invalid or failed extraction job")
		}
		if observe != nil {
			observe(*out.Progress)
		}
		if *out.Progress == 100 {
			if out.Status != "complete" || ParentID(out.Path) != parent {
				return Entry{}, errors.New("server did not confirm complete extraction into the selected folder")
			}
			return Entry{ID: out.Path, Name: name, Dir: true, Size: -1}, nil
		}
		if err := pollDelay(ctx); err != nil {
			return Entry{}, err
		}
	}
}

func objectID(id string) string { return id[strings.LastIndexByte(id, '/')+1:] }

func archiveError(err error) error {
	var api *APIError
	if !errors.As(err, &api) {
		return err
	}
	var detail string
	switch api.Code {
	case 400008002:
		detail = "server archive download size limit exceeded (official client reports 2 GB)"
	case 400008003:
		detail = "server archive extraction size limit exceeded"
	case 500008011:
		detail = "server failed to extract the archive"
	case 500008012:
		detail = "server extracted only part of the archive"
	case 403043013:
		detail = "source archive and extraction destination use different storage services"
	default:
		return err
	}
	return fmt.Errorf("%s: %w", detail, err)
}
func pollDelay(ctx context.Context) error {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type archiveWriter struct {
	dest        io.Writer
	done, total int64
	observe     func(ArchiveProgress)
}

func (w *archiveWriter) Write(p []byte) (int, error) {
	n, err := w.dest.Write(p)
	w.done += int64(n)
	w.report()
	return n, err
}
func (w *archiveWriter) report() {
	if w.observe != nil {
		w.observe(ArchiveProgress{Done: w.done, Total: w.total})
	}
}
