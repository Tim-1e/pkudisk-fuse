package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestUploadProgressTracksPartsAndWaitsForCommit(t *testing.T) {
	for _, failed := range []bool{false, true} {
		var output bytes.Buffer
		data := bytes.NewReader([]byte("multipart test bytes"))
		progress := &transferProgress{label: "Upload", total: data.Size(), output: &output, enabled: true}
		reader := progressReaderAt{reader: data, progress: progress}
		// Multipart uploads read separate ranges from the same ReaderAt.
		for offset := int64(0); offset < data.Size(); offset += 8 {
			n := min(int64(8), data.Size()-offset)
			if _, err := io.Copy(io.Discard, io.NewSectionReader(reader, offset, n)); err != nil {
				t.Fatal(err)
			}
		}
		if progress.done != data.Size() || strings.Contains(output.String(), "100%") {
			t.Fatal("upload was shown as committed before the server response")
		}
		var result error
		if failed {
			result = errors.New("server rejected upload publication")
		}
		progress.finish(result)
		text := output.String()
		if failed {
			if strings.Contains(text, "100%") || !strings.Contains(text, "failed") {
				t.Fatal("failed publication was shown as a successful upload")
			}
		} else if strings.Count(text, "100%") != 1 {
			t.Fatal("successful upload did not finish once at 100%")
		}
		progress.finish(result)
		if output.String() != text {
			t.Fatal("finishing twice duplicated the progress line")
		}
	}
}

type shortProgressWriter struct{}

func (shortProgressWriter) Write(p []byte) (int, error) {
	return len(p) / 2, errors.New("disk full")
}

func TestDownloadProgressCountsOnlyWrittenBytes(t *testing.T) {
	var output bytes.Buffer
	progress := &transferProgress{label: "Download", total: 8, output: &output, enabled: true}
	writer := progressWriter{writer: shortProgressWriter{}, progress: progress}
	n, err := writer.Write([]byte("12345678"))
	if err == nil || n != 4 || progress.done != 4 {
		t.Fatal("download progress included bytes that could not be written")
	}
	progress.finish(err)
	if strings.Contains(output.String(), "100%") || !strings.Contains(output.String(), "failed") {
		t.Fatal("disk failure was shown as a successful download")
	}
}

func TestEmptyTransferProgress(t *testing.T) {
	for _, failed := range []bool{false, true} {
		var output bytes.Buffer
		progress := &transferProgress{label: "Download", output: &output, enabled: true}
		var result error
		if failed {
			result = errors.New("download failed")
		}
		progress.finish(result)
		if strings.Contains(output.String(), "100%") == failed {
			t.Fatal("empty transfer completion did not match its result")
		}
	}
}
