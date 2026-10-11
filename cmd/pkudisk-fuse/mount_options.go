package main

import "time"

type mountOptions struct {
	mountpoint, root, cache       string
	readOnly, debug               bool
	cacheLimit, partSize          int64
	ttl, timeout, refreshInterval time.Duration
}
