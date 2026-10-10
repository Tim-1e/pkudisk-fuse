package anyshare

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
