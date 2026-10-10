// Package anyshare implements the HTTP protocol used by the PKU AnyShare client.
package anyshare

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrNotFound   = errors.New("remote entry not found")
	ErrExists     = errors.New("remote name already exists")
	ErrPermission = errors.New("remote permission denied")
	ErrConflict   = errors.New("remote revision changed")
	ErrQuota      = errors.New("remote quota exceeded")
	ErrSizeLimit  = errors.New("remote file size limit exceeded")
	ErrPending    = errors.New("operation requires server approval")
)

type Entry struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Rev      string    `json:"rev"`
	Type     string    `json:"type,omitempty"`
	Dir      bool      `json:"directory"`
	Modified time.Time `json:"modified_at"`
	Created  time.Time `json:"created_at"`
}

func (e *Entry) UnmarshalJSON(data []byte) error {
	type plain Entry
	var v struct {
		ID         string `json:"id"`
		DocID      string `json:"docid"`
		Name       string `json:"name"`
		Size       int64  `json:"size"`
		Rev        string `json:"rev"`
		Type       string `json:"type"`
		Modified   string `json:"modified_at"`
		Created    string `json:"created_at"`
		ModifiedUS int64  `json:"modified"`
		CreatedUS  int64  `json:"create_time"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	e.ID, e.Name, e.Size, e.Rev, e.Type = v.ID, v.Name, v.Size, v.Rev, v.Type
	if e.ID == "" {
		e.ID = v.DocID
	}
	e.Dir = v.Size == -1 || strings.HasSuffix(v.Type, "doc_lib") || v.Type == "folder" || v.Type == "dir"
	e.Modified, _ = time.Parse(time.RFC3339Nano, v.Modified)
	e.Created, _ = time.Parse(time.RFC3339Nano, v.Created)
	if e.Modified.IsZero() && v.ModifiedUS > 0 {
		e.Modified = time.UnixMicro(v.ModifiedUS)
	}
	if e.Created.IsZero() && v.CreatedUS > 0 {
		e.Created = time.UnixMicro(v.CreatedUS)
	}
	return nil
}

type APIError struct {
	Status     int
	Code       int
	Kind       error
	LimitBytes int64
}

func (e *APIError) Error() string {
	if errors.Is(e.Kind, ErrSizeLimit) && e.LimitBytes > 0 {
		return fmt.Sprintf("AnyShare HTTP %d, code %d: %v (limit: %d bytes)", e.Status, e.Code, e.Kind, e.LimitBytes)
	}
	return fmt.Sprintf("AnyShare HTTP %d, code %d: %v", e.Status, e.Code, e.Kind)
}
func (e *APIError) Unwrap() error { return e.Kind }

func responseError(status int, data []byte) error {
	var v struct {
		Code   int `json:"code"`
		Detail struct {
			FileLimitSize int64 `json:"file_limit_size"`
		} `json:"detail"`
	}
	_ = json.Unmarshal(data, &v)
	kind := errors.New("remote request failed")
	switch {
	case status == 202:
		kind = ErrPending
	case v.Code == 403002070:
		kind = ErrSizeLimit
	case v.Code == 403002040 || v.Code == 403002041 || v.Code == 403002039 || status == 507:
		kind = ErrQuota
	case v.Code == 403002015 || v.Code == 403002016 || status == 409 || status == 412:
		kind = ErrConflict
	case v.Code == 403002001 || v.Code == 403002002 || v.Code == 403002182:
		kind = ErrExists
	case status == 404 || v.Code == 404002005 || v.Code == 404002006:
		kind = ErrNotFound
	case status == 401 || status == 403:
		kind = ErrPermission
	}
	return &APIError{Status: status, Code: v.Code, Kind: kind, LimitBytes: max(0, v.Detail.FileLimitSize)}
}

// ValidName rejects paths, rather than letting an API call escape its parent.
func ValidName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && !strings.ContainsAny(name, "/\x00")
}

func LibraryID(id string) string {
	if !strings.HasPrefix(id, "gns://") {
		return ""
	}
	return "gns://" + strings.Split(strings.TrimPrefix(id, "gns://"), "/")[0]
}

func ParentID(id string) string {
	if id == LibraryID(id) {
		return ""
	}
	i := strings.LastIndexByte(id, '/')
	if i < 6 {
		return ""
	}
	return id[:i]
}
