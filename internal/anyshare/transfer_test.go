package anyshare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
			got, err := selectMultipartPartSize(test.size, test.configured, multipartLimits{MaxSize: maxInt64, MaxParts: maxMultipartParts})
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

func TestUploadUsesMultipartFlowForSmallNonEmptyFiles(t *testing.T) {
	data := []byte("small")
	var server *httptest.Server
	var endpoints []string
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoints = append(endpoints, r.URL.Path)
		switch r.URL.Path {
		case "/api/efast/v1/file/osoption":
			_ = json.NewEncoder(w).Encode(map[string]any{"partminsize": 5 << 20, "partmaxsize": 5 << 30, "partmaxnum": 10000})
		case "/api/efast/v1/file/osinitmultiupload":
			_ = json.NewEncoder(w).Encode(map[string]any{"docid": "gns://library/file", "name": "small.bin", "rev": "rev-1", "uploadid": "upload-1"})
		case "/api/efast/v1/file/osuploadpart":
			_ = json.NewEncoder(w).Encode(map[string]any{"authrequests": map[string]any{"1": []string{"PUT", server.URL + "/objects/1"}}})
		case "/objects/1":
			if r.Method != http.MethodPut {
				t.Errorf("part method = %s, want PUT", r.Method)
			}
			if got, err := io.ReadAll(r.Body); err != nil || !bytes.Equal(got, data) {
				t.Errorf("part body = %q, err = %v", got, err)
			}
			w.Header().Set("ETag", "etag-1")
			w.WriteHeader(http.StatusNoContent)
		case "/api/efast/v1/file/oscompleteupload":
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
	client.PartSize = 16 << 20
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
			t.Fatal("small upload used the single-request upload flow")
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
	if !strings.EqualFold(strings.Join(ranges, ","), strings.Join(wantRanges, ",")) {
		t.Fatalf("ranges = %v, want %v", ranges, wantRanges)
	}
}
