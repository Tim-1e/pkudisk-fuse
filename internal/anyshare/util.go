package anyshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Quota returns the server-side allocation for the authenticated account.
func (c *Client) Quota(ctx context.Context) (Quota, error) {
	var quota Quota
	b, err := c.request(ctx, "GET", "/efast/v1/quota/user", nil)
	if err != nil {
		return Quota{}, err
	}
	if err := json.Unmarshal(b, &quota); err != nil {
		return Quota{}, err
	}
	if quota.Allocated < 0 || quota.Used < 0 || quota.Used > quota.Allocated {
		return Quota{}, errors.New("server returned an invalid storage quota")
	}
	return quota, nil
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
			DocID     string `json:"doc_id"`
			Basename  string `json:"basename"`
			Extension string `json:"extension"`
			Size      int64  `json:"size"`
			Parent    string `json:"parent_path"`
			Modified  string `json:"modified_at"`
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
	result := SearchResult{Hits: response.Hits, Next: response.Next}
	for _, file := range response.Files {
		if file.DocID == "" || !ValidName(file.Basename) || file.Size < 0 {
			return SearchResult{}, errors.New("server returned an invalid search result")
		}
		name := file.Basename + file.Extension
		if !ValidName(name) {
			return SearchResult{}, errors.New("server returned an invalid search filename")
		}
		modified, _ := time.Parse(time.RFC3339Nano, file.Modified)
		result.Files = append(result.Files, SearchEntry{Entry: Entry{ID: file.DocID, Name: name, Size: file.Size, Modified: modified}, Path: file.Parent, Extension: file.Extension})
	}
	return result, nil
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
	if !source.Dir {
		if response.DocID == "" {
			return Entry{}, errors.New("server copy response omitted the new file ID")
		}
		return Entry{ID: response.DocID, Name: source.Name, Size: source.Size, Rev: response.Rev}, nil
	}
	if response.DocID != "" {
		return Entry{ID: response.DocID, Name: source.Name, Dir: true, Size: -1}, nil
	}
	if response.ID == "" {
		return Entry{}, errors.New("server directory copy response omitted a job ID")
	}
	for {
		var progress struct {
			Success  bool   `json:"success"`
			Status   string `json:"status"`
			Progress int    `json:"progress"`
			Error    any    `json:"error"`
		}
		if err := c.post(ctx, "/efast/v1/dir/copyprogress", map[string]string{"id": response.ID}, &progress); err != nil {
			return Entry{}, fmt.Errorf("poll server copy: %w", err)
		}
		if observe != nil {
			observe(CopyProgress{Percent: max(0, min(progress.Progress, 100)), Status: progress.Status})
		}
		if progress.Success && strings.EqualFold(progress.Status, "finished") {
			return Entry{ID: response.ID, Name: source.Name, Dir: true, Size: -1}, nil
		}
		if strings.EqualFold(progress.Status, "failed") || progress.Error != nil {
			return Entry{}, errors.New("server directory copy failed")
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Entry{}, ctx.Err()
		case <-timer.C:
		}
	}
}
