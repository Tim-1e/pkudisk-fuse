package vfs

import "errors"

// ReadOptions controls the latency/bandwidth tradeoff of mounted reads.
type ReadOptions struct {
	BlockSize   int64
	WholeFile   int64
	ReadAhead   int64
	Concurrency int
}

func DefaultReadOptions() ReadOptions {
	return ReadOptions{BlockSize: 4 << 20, WholeFile: 16 << 20, ReadAhead: 16 << 20, Concurrency: 4}
}

func (r ReadOptions) Validate() error {
	const maximum = 512 << 20
	if r.BlockSize <= 0 || r.BlockSize > maximum {
		return errors.New("--read-block-size must be between 1 byte and 512 MiB")
	}
	if r.WholeFile < 0 || r.WholeFile > maximum {
		return errors.New("--read-whole-file must be between 0 and 512 MiB")
	}
	if r.ReadAhead < 0 || r.ReadAhead > maximum {
		return errors.New("--read-ahead must be between 0 and 512 MiB")
	}
	if r.Concurrency <= 0 || r.Concurrency > 64 {
		return errors.New("--read-concurrency must be between 1 and 64")
	}
	return nil
}
