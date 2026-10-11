package main

import "testing"

func TestMountByteSizes(t *testing.T) {
	for text, want := range map[string]int64{
		"0": 0, "5368709121": (5 << 30) + 1, "8MiB": 8 << 20,
		"1.5GiB": 3 << 29, "5MB": 5000000, "9223372036854775807": 9223372036854775807,
	} {
		var got int64
		if err := (byteSize{&got}).Set(text); err != nil || got != want {
			t.Fatalf("size %s = %d, want %d; error %v", text, got, want, err)
		}
	}
	for _, text := range []string{"-1", "invalid", "8MiiB", "9223372036854775808", "18446744073709551615"} {
		var value int64
		if err := (byteSize{&value}).Set(text); err == nil {
			t.Fatalf("invalid size %s accepted", text)
		}
	}
}

func TestMountReadFlags(t *testing.T) {
	command := newRemoteCommand("mount")
	if err := command.ParseFlags([]string{"--read-whole-file", "32MiB", "--read-block-size", "1MiB", "--read-ahead", "64MiB", "--read-concurrency", "2", "--cache-limit", "2GiB"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int64{"read-whole-file": 32 << 20, "read-block-size": 1 << 20, "read-ahead": 64 << 20, "cache-limit": 2 << 30} {
		value := command.Flags().Lookup(name).Value.(byteSize)
		if *value.value != want {
			t.Fatalf("--%s parsed as %d", name, *value.value)
		}
	}
	if value, err := command.Flags().GetInt("read-concurrency"); err != nil || value != 2 {
		t.Fatal("read concurrency was not parsed", value, err)
	}
}
