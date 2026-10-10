package anyshare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type signedRequest []string
type uploadStart struct {
	ID       string        `json:"docid"`
	Name     string        `json:"name"`
	Rev      string        `json:"rev"`
	UploadID string        `json:"uploadid"`
	Request  signedRequest `json:"authrequest"`
}

func (c *Client) signed(ctx context.Context, auth signedRequest, body io.Reader, length int64, headers http.Header) (*http.Response, error) {
	if len(auth) < 2 {
		return nil, errors.New("invalid signed request")
	}
	u, err := url.Parse(auth[1])
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("invalid object storage URL")
	}
	if strings.HasPrefix(c.base, "https:") && u.Scheme != "https" {
		return nil, errors.New("server returned an insecure object storage URL")
	}
	req, err := http.NewRequestWithContext(ctx, auth[0], auth[1], body)
	if err != nil {
		return nil, errors.New("invalid object storage request")
	}
	for _, line := range auth[2:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, errors.New("invalid signed header")
		}
		if strings.EqualFold(key, "x-as-userid") {
			continue
		}
		req.Header.Set(key, strings.TrimSpace(value))
	}
	for key, values := range headers {
		req.Header[key] = values
	}
	if body != nil {
		req.ContentLength = length
	}
	resp, err := c.objects.Do(req)
	if err != nil {
		return nil, safeTransportError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("object storage HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (c *Client) downloadRequest(ctx context.Context, e Entry) (signedRequest, error) {
	var out struct {
		Request signedRequest `json:"authrequest"`
	}
	err := c.post(ctx, "/efast/v1/file/osdownload", map[string]any{"docid": e.ID, "rev": e.Rev, "authtype": "QUERY_STRING", "usehttps": strings.HasPrefix(c.base, "https:")}, &out)
	return out.Request, err
}

func (c *Client) ReadAt(ctx context.Context, e Entry, dest []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative read offset")
	}
	if off >= e.Size || len(dest) == 0 {
		return 0, nil
	}
	end := off + int64(len(dest)) - 1
	if end >= e.Size {
		end = e.Size - 1
	}
	auth, err := c.downloadRequest(ctx, e)
	if err != nil {
		return 0, err
	}
	resp, err := c.signed(ctx, auth, nil, 0, http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", off, end)}})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPartialContent {
		var first, last, total int64
		if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &first, &last, &total); err != nil || first != off || last != end || total != e.Size {
			return 0, errors.New("object storage returned an inconsistent byte range")
		}
	} else if resp.StatusCode != 200 || off != 0 {
		return 0, errors.New("object storage did not honor the requested byte range")
	}
	n, err := io.ReadFull(resp.Body, dest[:end-off+1])
	return n, err
}

func (c *Client) Download(ctx context.Context, e Entry, dest io.Writer) error {
	if e.Size == 0 {
		return nil
	}
	auth, err := c.downloadRequest(ctx, e)
	if err != nil {
		return err
	}
	resp, err := c.signed(ctx, auth, nil, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("unexpected full download response")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != e.Size {
		return errors.New("download length differs from the selected revision")
	}
	n, err := io.Copy(dest, io.LimitReader(resp.Body, e.Size+1))
	if err != nil {
		return err
	}
	if n != e.Size {
		return errors.New("incomplete or oversized download")
	}
	return nil
}

// Upload creates an entry, or edits a specific existing revision. Data is streamed,
// never held as a whole-file memory buffer. The API publishes it only at osendupload.
func (c *Client) Upload(ctx context.Context, parent, name string, existing *Entry, data io.ReaderAt, size int64, mtime time.Time) (Entry, error) {
	if !ValidName(name) || size < 0 {
		return Entry{}, errors.New("invalid upload metadata")
	}
	body := map[string]any{"docid": parent, "name": name, "length": size, "ondup": 1, "client_mtime": mtime.UnixMicro()}
	if existing != nil {
		fresh, err := c.Stat(ctx, *existing)
		if err != nil {
			return Entry{}, err
		}
		if existing.Rev == "" || fresh.Rev != existing.Rev {
			return Entry{}, ErrConflict
		}
		body["docid"], body["editedrev"] = existing.ID, existing.Rev
		delete(body, "name")
	}
	var start uploadStart
	var err error
	if size > c.PartSize && c.PartSize > 0 {
		err = c.post(ctx, "/efast/v1/file/osinitmultiupload", body, &start)
		if err == nil {
			err = c.uploadParts(ctx, start, data, size)
		}
	} else {
		body["reqmethod"] = "POST"
		err = c.post(ctx, "/efast/v1/file/osbeginupload", body, &start)
		if err == nil {
			err = c.uploadSingle(ctx, start, data, size)
		}
	}
	if err != nil {
		return Entry{}, err
	}
	if start.ID == "" || start.Rev == "" {
		return Entry{}, errors.New("upload response omitted ID or revision")
	}
	if err = c.post(ctx, "/efast/v1/file/osendupload", map[string]any{"docid": start.ID, "rev": start.Rev}, nil); err != nil {
		return Entry{}, err
	}
	return Entry{ID: start.ID, Name: name, Rev: start.Rev, Size: size}, nil
}

func (c *Client) uploadSingle(ctx context.Context, start uploadStart, data io.ReaderAt, size int64) error {
	if len(start.Request) < 2 || start.Request[0] != "POST" {
		return errors.New("unexpected single upload request")
	}
	// Build only the small MIME envelope. A known Content-Length is needed by
	// some S3-compatible stores; the file body still streams from ReaderAt.
	var envelope bytes.Buffer
	mw := multipart.NewWriter(&envelope)
	for _, line := range start.Request[2:] {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return errors.New("invalid upload form field")
		}
		if err := mw.WriteField(key, strings.TrimSpace(val)); err != nil {
			return err
		}
	}
	if _, err := mw.CreateFormFile("file", start.Name); err != nil {
		return err
	}
	prefixLen := envelope.Len()
	if err := mw.Close(); err != nil {
		return err
	}
	prefix, suffix := envelope.Bytes()[:prefixLen], envelope.Bytes()[prefixLen:]
	body := io.MultiReader(bytes.NewReader(prefix), io.NewSectionReader(data, 0, size), bytes.NewReader(suffix))
	resp, err := c.signed(ctx, start.Request[:2], body, int64(envelope.Len())+size, http.Header{"Content-Type": {mw.FormDataContentType()}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return err
}

func (c *Client) uploadParts(ctx context.Context, start uploadStart, data io.ReaderAt, size int64) error {
	if start.UploadID == "" || start.ID == "" || start.Rev == "" {
		return errors.New("incomplete multipart upload response")
	}
	count := (size + c.PartSize - 1) / c.PartSize
	if count > 10000 {
		return errors.New("file exceeds the multipart part limit; increase --part-size")
	}
	info := map[string]any{}
	for part := int64(1); part <= count; part++ {
		var auth struct {
			Requests map[string]signedRequest `json:"authrequests"`
		}
		if err := c.post(ctx, "/efast/v1/file/osuploadpart", map[string]any{"docid": start.ID, "rev": start.Rev, "uploadid": start.UploadID, "parts": strconv.FormatInt(part, 10), "usehttps": strings.HasPrefix(c.base, "https:")}, &auth); err != nil {
			return err
		}
		off := (part - 1) * c.PartSize
		n := min(c.PartSize, size-off)
		resp, err := c.signed(ctx, auth.Requests[strconv.FormatInt(part, 10)], io.NewSectionReader(data, off, n), n, nil)
		if err != nil {
			return err
		}
		etag := resp.Header.Get("ETag")
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if etag == "" {
			return errors.New("object storage omitted the uploaded part ETag")
		}
		info[strconv.FormatInt(part, 10)] = []any{etag, n}
	}
	b, err := c.request(ctx, http.MethodPost, "/efast/v1/file/oscompleteupload", map[string]any{"docid": start.ID, "rev": start.Rev, "uploadid": start.UploadID, "partinfo": info})
	if err != nil {
		return err
	}
	return c.completeMultipart(ctx, b)
}

// oscompleteupload returns a MIME envelope containing completion XML and a
// second JSON part with a signed request. Replay that XML verbatim, then publish.
func (c *Client) completeMultipart(ctx context.Context, b []byte) error {
	trim := bytes.TrimLeft(b, "\r\n\t ")
	line, _, ok := bytes.Cut(trim, []byte("\n"))
	if !ok {
		return errors.New("invalid multipart completion envelope")
	}
	boundary := strings.TrimSpace(string(line))
	if !strings.HasPrefix(boundary, "--") {
		return errors.New("missing multipart completion boundary")
	}
	var xml []byte
	var auth struct {
		Request signedRequest `json:"authrequest"`
	}
	chunks := bytes.Split(trim, []byte(boundary))
	if len(chunks) == 4 && bytes.HasPrefix(bytes.TrimSpace(chunks[1]), []byte("<")) {
		// The official client also accepts a bare boundary envelope without MIME headers.
		xml = bytes.TrimSpace(chunks[1])
		if err := json.Unmarshal(bytes.TrimSpace(chunks[2]), &auth); err != nil {
			return err
		}
	} else {
		mr := multipart.NewReader(bytes.NewReader(trim), strings.TrimPrefix(boundary, "--"))
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(p, 8<<20))
			if err != nil {
				return err
			}
			typ, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
			if typ == "application/json" || json.Valid(bytes.TrimSpace(data)) {
				if err := json.Unmarshal(data, &auth); err != nil {
					return err
				}
			} else {
				xml = data
			}
		}
	}
	if len(xml) == 0 || len(auth.Request) < 2 {
		return errors.New("incomplete multipart completion envelope")
	}
	resp, err := c.signed(ctx, auth.Request, bytes.NewReader(xml), int64(len(xml)), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	result, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	// S3 can return HTTP 200 with an XML Error instead of completing the object.
	if bytes.Contains(result, []byte("<Error>")) || bytes.Contains(result, []byte("<Error ")) {
		return errors.New("object storage rejected multipart completion")
	}
	return nil
}
