package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
)

func TestListingsAlignWideNames(t *testing.T) {
	var output bytes.Buffer
	entries := []anyshare.Entry{
		{Name: "12TB_文档库", Type: "custom_doc_lib", Size: -1, ID: "gns://custom"},
		{Name: "北大网盘公共资源", Type: "department_doc_lib", Size: -1, ID: "gns://public"},
		{Name: "🖼️ Photos é", Type: "user_doc_lib", Size: 12345, ID: "gns://user"},
	}
	if err := printEntries(&output, entries, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatal(output.String())
	}
	expected := ansi.StringWidth(lines[0][:strings.Index(lines[0], "ID")])
	for _, line := range lines[1:] {
		at := strings.Index(line, "gns://")
		if at < 0 || ansi.StringWidth(line[:at]) != expected {
			t.Fatal("columns use character counts instead of display widths", output.String())
		}
	}
	if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), "\t") {
		t.Fatal("listing contains ANSI codes or tab stops")
	}
}

func TestSharedTableAlignsNumericColumnsAndSanitizesCells(t *testing.T) {
	var output bytes.Buffer
	if err := printTable(&output, []string{"NAME", "BYTES", "ID"}, [][]string{{"中文", "1", "first"}, {"emoji😀", "1000000", "second"}, {"line\n\x1b[31m", "-1", "third"}}, 1); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 4 || strings.Contains(output.String(), "\x1b") {
		t.Fatal("control characters broke the table")
	}
	edge := -1
	for i, cell := range []string{"1", "1000000", "-1"} {
		line := lines[i+1]
		at := strings.LastIndex(line, cell)
		width := ansi.StringWidth(line[:at+len(cell)])
		if edge < 0 {
			edge = width
		}
		if width != edge {
			t.Fatal("numeric column is not right aligned", output.String())
		}
	}
}
