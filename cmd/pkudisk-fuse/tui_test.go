package main

import (
	"os"
	"testing"
)

func TestTUITextRemovesTerminalControls(t *testing.T) {
	if got := tuiText("name\x1b[31m\n"); got != "name?[31m?" {
		t.Fatalf("tuiText = %q", got)
	}
}

func TestTUIReadKeyMapsArrowSequence(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	if _, err := write.Write([]byte("\x1b[A")); err != nil {
		t.Fatal(err)
	}
	key, err := tuiReadKey(int(read.Fd()))
	if err != nil || key != 'k' {
		t.Fatalf("key = %q, err = %v", key, err)
	}
}
