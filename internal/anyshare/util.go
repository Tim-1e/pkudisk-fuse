package anyshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// Quota aggregates the libraries accessible to the authenticated account.
func (c *Client) Quota(ctx context.Context) (Quota, error) {
	return c.StorageQuota(ctx, "", false)
}

// LibraryQuota reads the allocation and usage of one library, including shared
// custom/department libraries that do not consume the user's personal quota.
func (c *Client) LibraryQuota(ctx context.Context, id string) (Quota, error) {
	var response struct {
		Allocated *int64 `json:"allocated"`
		Used      *int64 `json:"used"`
	}
	b, err := c.request(ctx, "GET", "/efast/v1/quota/doc-lib/"+url.PathEscape(id), nil)
	if err != nil {
		return Quota{}, err
	}
	if err := json.Unmarshal(b, &response); err != nil {
		return Quota{}, err
	}
	if response.Allocated == nil || response.Used == nil || *response.Allocated < 0 || *response.Used < 0 {
		return Quota{}, errors.New("server returned an invalid storage quota")
	}
	return Quota{Allocated: *response.Allocated, Used: *response.Used}, nil
}

// StorageQuota describes mounted capacity. Read-only libraries contribute only
// their used bytes to both counters. An optional root restricts the sum to its
// containing library and checks write permission at that selected folder.
func (c *Client) StorageQuota(ctx context.Context, root string, readOnly bool) (Quota, error) {
	roots, err := c.Roots(ctx)
	if err != nil {
		return Quota{}, err
	}
	var total Quota
	for _, library := range roots {
		if root != "" && library.ID != LibraryID(root) {
			continue
		}
		quota, err := c.LibraryQuota(ctx, library.ID)
		if err != nil {
			return Quota{}, fmt.Errorf("read library quota: %w", err)
		}
		writable := false
		if !readOnly {
			selector := library.ID
			if root != "" {
				selector = root
			}
			writable, err = c.Writable(ctx, selector)
			if err != nil {
				return Quota{}, fmt.Errorf("check library write permission: %w", err)
			}
		}
		if !writable {
			quota.Allocated = quota.Used
		} else {
			quota.Allocated = max(quota.Allocated, quota.Used)
		}
		if total.Allocated > math.MaxInt64-quota.Allocated || total.Used > math.MaxInt64-quota.Used {
			return Quota{}, errors.New("combined library quota exceeds the supported byte count")
		}
		total.Allocated += quota.Allocated
		total.Used += quota.Used
	}
	return total, nil
}

type SearchEntry struct {
	Entry
	Path      string `json:"path"`
	Extension string `json:"extension,omitempty"`
}

type SearchResult struct {
	Files []SearchEntry
	Hits  int
	Next  int
}

// Search performs one bounded server-side filename search. A non-empty scope
// is a checked gns:// folder ID; the server treats its descendants as a tree.
func (c *Client) Search(ctx context.Context, keyword, scope string, limit int) (SearchResult, error) {
	if strings.TrimSpace(keyword) == "" {
		return SearchResult{}, errors.New("search keyword cannot be empty")
	}
	if len(keyword) > 1024 {
		return SearchResult{}, errors.New("search keyword is too long")
	}
	if limit < 1 || limit > 1000 {
		return SearchResult{}, errors.New("search limit must be between 1 and 1000")
	}
	ranges := []string{}
	if scope != "" {
		if !strings.HasPrefix(scope, "gns://") || strings.ContainsAny(scope, "\x00\r\n") {
			return SearchResult{}, errors.New("search scope must be a gns:// folder ID")
		}
		ranges = []string{strings.TrimRight(scope, "/") + "*"}
	}
	var response struct {
		Files []struct {
			DocID     string          `json:"doc_id"`
			Basename  string          `json:"basename"`
			Extension string          `json:"extension"`
			Size      int64           `json:"size"`
			Parent    string          `json:"parent_path"`
			Modified  json.RawMessage `json:"modified_at"`
			Created   json.RawMessage `json:"created_at"`
		} `json:"files"`
		Hits int `json:"hits"`
		Next int `json:"next"`
	}
	body := map[string]any{
		"quick_search": true,
		"dimension":    []string{"basename"},
		"keyword":      keyword,
		"rows":         limit,
		"range":        ranges,
		"start":        0,
		"type":         "doc",
	}
	if err := c.post(ctx, "/ecosearch/v1/file-search", body, &response); err != nil {
		return SearchResult{}, err
	}
	result := SearchResult{Files: []SearchEntry{}, Hits: response.Hits, Next: response.Next}
	for _, file := range response.Files {
		if file.DocID == "" || file.Size < -1 {
			return SearchResult{}, errors.New("server returned an invalid search result")
		}
		name := file.Basename + file.Extension
		if !ValidName(name) {
			return SearchResult{}, errors.New("server returned an invalid search filename")
		}
		modified, err := searchTime(file.Modified)
		if err != nil {
			return SearchResult{}, fmt.Errorf("server returned an invalid modified_at timestamp: %w", err)
		}
		created, err := searchTime(file.Created)
		if err != nil {
			return SearchResult{}, fmt.Errorf("server returned an invalid created_at timestamp: %w", err)
		}
		result.Files = append(result.Files, SearchEntry{Entry: Entry{ID: file.DocID, Name: name, Size: file.Size, Dir: file.Size == -1, Modified: modified, Created: created}, Path: file.Parent, Extension: file.Extension})
	}
	return result, nil
}

func searchTime(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, nil
	}
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return time.Time{}, err
		}
		if value == "" {
			return time.Time{}, nil
		}
		return time.Parse(time.RFC3339Nano, value)
	}
	var micros int64
	if err := json.Unmarshal(raw, &micros); err != nil {
		return time.Time{}, err
	}
	return time.UnixMicro(micros).UTC(), nil
}

type CopyProgress struct {
	Percent int
	Status  string
}

// Copy asks the server to duplicate an existing file or directory under a
// destination folder. Directory copies may be asynchronous; the method polls
// the official progress endpoint until the server reports success or failure.
func (c *Client) Copy(ctx context.Context, source, destination Entry, observe func(CopyProgress)) (Entry, error) {
	if source.ID == "" || destination.ID == "" || !destination.Dir {
		return Entry{}, errors.New("copy requires a source and destination folder")
	}
	if source.Dir && (source.ID == destination.ID || strings.HasPrefix(destination.ID, source.ID+"/")) {
		return Entry{}, errors.New("copy destination cannot be inside the source directory")
	}
	ondup := 1
	if ParentID(source.ID) == destination.ID {
		// The official client uses 2 for copying within the same folder.
		ondup = 2
	}
	body := map[string]any{"docid": source.ID, "destparent": destination.ID, "ondup": ondup}
	path := "/efast/v1/file/copy"
	if source.Dir {
		path = "/efast/v1/dir/copy"
		body["check_upload_process"] = true
	}
	var response struct {
		DocID  string `json:"docid"`
		ID     string `json:"id"`
		Name   string `json:"name"`
		Rev    string `json:"rev"`
		Size   int64  `json:"size"`
		Status string `json:"status"`
	}
	if err := c.post(ctx, path, body, &response); err != nil {
		return Entry{}, fmt.Errorf("server copy: %w", err)
	}
	if response.DocID == "" || ParentID(response.DocID) != destination.ID {
		return Entry{}, errors.New("server copy response omitted a valid destination ID")
	}
	// A directory response contains both the new docid and the async job id.
	// The docid already exists while members are still being copied.
	for source.Dir && response.ID != "" {
		var progress struct {
			Success    bool   `json:"success"`
			Status     string `json:"status"`
			Size       int64  `json:"size"`
			SizeCopied int64  `json:"sizecopied"`
			FileCount  int64  `json:"filecount"`
			FileCopied int64  `json:"filecopied"`
			DirCount   int64  `json:"dircount"`
			DirCopied  int64  `json:"dircopied"`
			Error      any    `json:"error"`
		}
		if err := c.post(ctx, "/efast/v1/dir/copyprogress", map[string]string{"id": response.ID}, &progress); err != nil {
			return Entry{}, fmt.Errorf("poll server copy: %w", err)
		}
		if observe != nil {
			total, done := progress.Size, progress.SizeCopied
			if total <= 0 {
				total, done = progress.FileCount+progress.DirCount, progress.FileCopied+progress.DirCopied
			}
			percent := 0
			if total > 0 {
				percent = int(100 * float64(done) / float64(total))
			}
			if progress.Status == "finished" {
				percent = 100
			}
			observe(CopyProgress{Percent: max(0, min(percent, 100)), Status: progress.Status})
		}
		if !progress.Success || progress.Status == "failed" || progress.Error != nil {
			return Entry{}, errors.New("server directory copy failed")
		}
		if progress.Status == "finished" {
			break
		}
		if progress.Status != "waiting" && progress.Status != "scanning" && progress.Status != "copying" {
			return Entry{}, errors.New("server returned an unknown directory copy state")
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Entry{}, ctx.Err()
		case <-timer.C:
		}
	}
	entry := Entry{ID: response.DocID, Name: source.Name, Dir: source.Dir, Size: source.Size, Rev: response.Rev}
	// Same-folder copies are renamed by the server; use their real name.
	if ondup == 2 {
		return c.Stat(ctx, entry)
	}
	return entry, nil
}
