package anyshare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestResponseErrorReportsFileSizeLimit(t *testing.T) {
	err := responseError(403, []byte(`{"code":403002070,"detail":{"file_limit_size":104857600}}`))
	if !errors.Is(err, ErrSizeLimit) {
		t.Fatalf("error = %v, want ErrSizeLimit", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.LimitBytes != 104857600 {
		t.Fatalf("API error = %#v, want 100 MiB limit", apiErr)
	}
	if !strings.Contains(err.Error(), "104857600") {
		t.Fatalf("error omitted the server limit: %v", err)
	}
}

func TestSelectMultipartPartSizeKeepsPartCountWithinLimit(t *testing.T) {
	const maxMultipartParts int64 = 10000
	tests := []struct {
		name       string
		size       int64
		configured int64
		want       int64
	}{
		{name: "normal", size: 64 << 20, configured: 16 << 20, want: 16 << 20},
		{name: "large", size: maxMultipartParts*(16<<20) + 1, configured: 16 << 20, want: 16<<20 + 1},
		{name: "configured larger", size: maxMultipartParts*(16<<20) + 1, configured: 32 << 20, want: 32 << 20},
		{name: "max int64", size: int64(^uint64(0) >> 1), configured: 16 << 20, want: (int64(^uint64(0)>>1) / maxMultipartParts) + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectMultipartPartSize(test.size, test.configured, multipartLimits{MinSize: 1, MaxSize: int64(^uint64(0) >> 1), MaxParts: maxMultipartParts})
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("selectMultipartPartSize(%d, %d) = %d, want %d", test.size, test.configured, got, test.want)
			}
			count := test.size / got
			if test.size%got != 0 {
				count++
			}
			if count > maxMultipartParts {
				t.Fatalf("part count %d exceeds limit %d", count, maxMultipartParts)
			}
		})
	}
}

func TestSelectMultipartPartSizeUsesServerLimits(t *testing.T) {
	limits := multipartLimits{MinSize: 5, MaxSize: 100, MaxParts: 3}
	tests := []struct {
		name       string
		size       int64
		configured int64
		want       int64
		wantErr    bool
	}{
		{name: "minimum", size: 1, configured: 1, want: 5},
		{name: "configured", size: 10, configured: 20, want: 20},
		{name: "part count", size: 250, configured: 20, want: 84},
		{name: "maximum configured", size: 10, configured: 200, want: 100},
		{name: "capacity", size: 301, configured: 20, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectMultipartPartSize(test.size, test.configured, limits)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr=%v", err, test.wantErr)
			}
			if !test.wantErr && got != test.want {
				t.Fatalf("part size = %d, want %d", got, test.want)
			}
		})
	}
}

func TestMultipartCapacityBoundary(t *testing.T) {
	limits := multipartLimits{MinSize: 20 << 20, MaxSize: 5 << 30, MaxParts: 10000}
	capacity := limits.MaxSize * limits.MaxParts
	partSize, err := selectMultipartPartSize(capacity, 16<<20, limits)
	if err != nil || partSize != limits.MaxSize {
		t.Fatalf("capacity boundary: part size=%d error=%v", partSize, err)
	}
	_, err = selectMultipartPartSize(capacity+1, 16<<20, limits)
	if !errors.Is(err, ErrSizeLimit) || !strings.Contains(err.Error(), strconv.FormatInt(capacity, 10)) {
		t.Fatalf("over capacity: %v", err)
	}
	for _, bad := range []multipartLimits{
		{MinSize: 0, MaxSize: 5, MaxParts: 10},
		{MinSize: 1, MaxSize: 0, MaxParts: 10},
		{MinSize: 1, MaxSize: 5, MaxParts: 0},
		{MinSize: 6, MaxSize: 5, MaxParts: 10},
	} {
		if _, err := selectMultipartPartSize(1, 1, bad); err == nil {
			t.Fatalf("accepted invalid server limits: %+v", bad)
		}
	}
}

func TestUploadRejectsCapacityBeforeCreatingRemoteFile(t *testing.T) {
	var endpoints []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoints = append(endpoints, r.URL.Path)
		if r.URL.Path != "/api/efast/v1/file/osoption" {
			t.Errorf("over-capacity upload issued %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(multipartLimits{MinSize: 20 << 20, MaxSize: 5 << 30, MaxParts: 10000})
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "test-access"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Upload(context.Background(), "gns://library", "too-large.bin", nil, bytes.NewReader(nil), (5<<30)*10000+1, time.Now())
	if !errors.Is(err, ErrSizeLimit) || len(endpoints) != 1 {
		t.Fatalf("error=%v endpoints=%v", err, endpoints)
	}
}

func TestUploadUsesKnownContentLength(t *testing.T) {
	data := []byte("sample file bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength <= int64(len(data)) {
			t.Error("multipart upload omitted the form length")
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		fields := map[string]string{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			b, err := io.ReadAll(part)
			if err != nil {
				t.Error(err)
				return
			}
			fields[part.FormName()] = string(b)
		}
		if fields["key"] != "sample-key" || fields["file"] != string(data) {
			t.Error("upload form lost a field or file bytes")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "test-access"})
	if err != nil {
		t.Fatal(err)
	}
	start := uploadStart{Name: "file.txt", Request: signedRequest{"POST", server.URL, "key: sample-key"}}
	if err = client.uploadSingle(context.Background(), start, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

func TestUploadUsesNegotiatedMultipartFlowWhenNeeded(t *testing.T) {
	data := []byte("a file that requires larger parts automatically!")
	const partSize = 16
	var server *httptest.Server
	var endpoints []string
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoints = append(endpoints, r.URL.Path)
		switch r.URL.Path {
		case "/api/efast/v1/file/osoption":
			_ = json.NewEncoder(w).Encode(multipartLimits{MinSize: 5, MaxSize: 20, MaxParts: 3})
		case "/api/efast/v1/file/osinitmultiupload":
			_ = json.NewEncoder(w).Encode(map[string]any{"docid": "gns://library/file", "name": "small.bin", "rev": "rev-1", "uploadid": "upload-1"})
		case "/api/efast/v1/file/osuploadpart":
			var request struct {
				Parts string `json:"parts"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"authrequests": map[string]any{request.Parts: []string{"PUT", server.URL + "/objects/" + request.Parts}}})
		case "/objects/1", "/objects/2", "/objects/3":
			part := strings.TrimPrefix(r.URL.Path, "/objects/")
			partNumber := int(part[0] - '0')
			start := (partNumber - 1) * partSize
			want := data[start:min(start+partSize, len(data))]
			if r.Method != http.MethodPut {
				t.Errorf("part method = %s, want PUT", r.Method)
			}
			if got, err := io.ReadAll(r.Body); err != nil || !bytes.Equal(got, want) {
				t.Errorf("part body = %q, want %q, err = %v", got, want, err)
			}
			w.Header().Set("ETag", "etag-"+part)
			w.WriteHeader(http.StatusNoContent)
		case "/api/efast/v1/file/oscompleteupload":
			var request struct {
				Info map[string][]json.RawMessage `json:"partinfo"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(request.Info) != 3 {
				t.Errorf("completed %d parts, want 3", len(request.Info))
			}
			for part := 1; part <= 3; part++ {
				info := request.Info[strconv.Itoa(part)]
				if len(info) != 2 {
					t.Errorf("invalid completed part %d: %v", part, info)
					continue
				}
				var etag string
				var length int64
				_ = json.Unmarshal(info[0], &etag)
				_ = json.Unmarshal(info[1], &length)
				if etag != "etag-"+strconv.Itoa(part) || length != int64(min(partSize, len(data)-(part-1)*partSize)) {
					t.Errorf("completed part %d has etag=%q length=%d", part, etag, length)
				}
			}
			auth, _ := json.Marshal(map[string]any{"authrequest": []string{"POST", server.URL + "/objects/complete"}})
			w.Write([]byte("--completion\r\n<CompleteMultipartUpload/>\r\n--completion\r\n"))
			w.Write(auth)
			w.Write([]byte("\r\n--completion--\r\n"))
		case "/objects/complete":
			w.Write([]byte("<CompleteMultipartUploadResult/>"))
		case "/api/efast/v1/file/osendupload":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "test-access"})
	if err != nil {
		t.Fatal(err)
	}
	client.PartSize = 4
	entry, err := client.Upload(context.Background(), "gns://library", "small.bin", nil, bytes.NewReader(data), int64(len(data)), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if entry.Size != int64(len(data)) {
		t.Fatalf("entry size = %d, want %d", entry.Size, len(data))
	}
	seenOptions, seenMultipart := false, false
	for _, endpoint := range endpoints {
		seenOptions = seenOptions || endpoint == "/api/efast/v1/file/osoption"
		seenMultipart = seenMultipart || endpoint == "/api/efast/v1/file/osinitmultiupload"
		if endpoint == "/api/efast/v1/file/osbeginupload" {
			t.Fatal("multipart upload used the single-request upload flow")
		}
	}
	if !seenOptions || !seenMultipart {
		t.Fatalf("multipart negotiation endpoints missing: %v", endpoints)
	}
}

func TestCompleteBareMultipartEnvelope(t *testing.T) {
	xml := []byte("<CompleteMultipartUpload><Part/></CompleteMultipartUpload>")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if !bytes.Equal(b, xml) {
			t.Error("multipart completion XML changed")
		}
		w.Write([]byte("<CompleteMultipartUploadResult/>"))
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "test-access"})
	if err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(map[string]any{"authrequest": []string{"POST", server.URL}})
	envelope := append([]byte("--completion\r\n"), xml...)
	envelope = append(envelope, []byte("\r\n--completion\r\n")...)
	envelope = append(envelope, auth...)
	envelope = append(envelope, []byte("\r\n--completion--\r\n")...)
	if err = client.completeMultipart(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadUsesBoundedRanges(t *testing.T) {
	data := []byte("0123456789abcdef")
	var ranges []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/efast/v1/file/osdownload":
			_ = json.NewEncoder(w).Encode(map[string]any{"authrequest": []string{"GET", "http://" + r.Host + "/objects/file"}})
		case "/objects/file":
			rangeHeader := r.Header.Get("Range")
			ranges = append(ranges, rangeHeader)
			var first, last int
			if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &first, &last); err != nil || first < 0 || last < first || last >= len(data) {
				http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			body := data[first : last+1]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "test-access"})
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	entry := Entry{ID: "gns://library/file", Rev: "rev-1", Size: int64(len(data))}
	if err := client.downloadRanges(context.Background(), entry, &got, 5); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("downloaded %q, want %q", got.Bytes(), data)
	}
	wantRanges := []string{"bytes=0-4", "bytes=5-9", "bytes=10-14", "bytes=15-15"}
	if strings.Join(ranges, ",") != strings.Join(wantRanges, ",") {
		t.Fatalf("ranges = %v, want %v", ranges, wantRanges)
	}
}

func TestDownloadValidatesRangeAndBody(t *testing.T) {
	for _, test := range []struct {
		name, contentRange, body string
		status                   int
		wantError                bool
	}{
		{name: "exact range", contentRange: "bytes 0-3/4", body: "test", status: 206},
		{name: "whole small file", body: "test", status: 200},
		{name: "missing range", body: "test", status: 206, wantError: true},
		{name: "wrong offset", contentRange: "bytes 1-4/4", body: "test", status: 206, wantError: true},
		{name: "wrong total", contentRange: "bytes 0-3/5", body: "test", status: 206, wantError: true},
		{name: "short body", contentRange: "bytes 0-3/4", body: "tes", status: 206, wantError: true},
		{name: "extra byte", contentRange: "bytes 0-3/4", body: "tests", status: 206, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/efast/v1/file/osdownload" {
					_ = json.NewEncoder(w).Encode(map[string]any{"authrequest": []string{"GET", "http://" + r.Host + "/object"}})
					return
				}
				w.Header().Set("Content-Range", test.contentRange)
				w.WriteHeader(test.status)
				w.(http.Flusher).Flush() // Exercise byte counting without Content-Length.
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client, err := New(server.URL, &testTokenSource{token: "test-access"})
			if err != nil {
				t.Fatal(err)
			}
			var dest bytes.Buffer
			err = client.Download(context.Background(), Entry{ID: "gns://library/file", Rev: "revision", Size: 4}, &dest)
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v wantError=%v", err, test.wantError)
			}
			if dest.Len() > 4 || (!test.wantError && dest.String() != "test") {
				t.Fatalf("downloaded %q", dest.String())
			}
		})
	}
}

func TestReadAtClampsWithoutOffsetOverflow(t *testing.T) {
	const total = int64(^uint64(0) >> 1)
	const off = total - 3
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/efast/v1/file/osdownload" {
			_ = json.NewEncoder(w).Encode(map[string]any{"authrequest": []string{"GET", "http://" + r.Host + "/object"}})
			return
		}
		if got := r.Header.Get("Range"); got != fmt.Sprintf("bytes=%d-%d", off, total-1) {
			t.Errorf("range overflowed: %s", got)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, total-1, total))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "end")
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "test-access"})
	if err != nil {
		t.Fatal(err)
	}
	dest := make([]byte, 8)
	n, err := client.ReadAt(context.Background(), Entry{ID: "gns://library/file", Size: total}, dest, off)
	if err != nil || n != 3 || string(dest[:n]) != "end" {
		t.Fatalf("read at int64 boundary: bytes=%q n=%d error=%v", dest[:n], n, err)
	}
}
