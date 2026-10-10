package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

type transferProgress struct {
	label   string
	total   int64
	done    int64
	output  io.Writer
	enabled bool
	mu      sync.Mutex
	last    time.Time
	ended   bool
}

func newTransferProgress(label string, total int64) *transferProgress {
	p := &transferProgress{label: label, total: max(total, 0), output: os.Stderr, enabled: term.IsTerminal(int(os.Stderr.Fd()))}
	if p.enabled {
		p.last = time.Now()
		p.renderLocked(false, false)
	}
	return p
}

func (p *transferProgress) add(n int) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return
	}
	p.done += min(int64(n), p.total-p.done)
	if !p.enabled || (p.done < p.total && time.Since(p.last) < 100*time.Millisecond) {
		return
	}
	p.last = time.Now()
	p.renderLocked(false, false)
}

// The transfer is complete only after the remote upload is published or the
// verified download has been installed at its destination. Keep the bar below
// 100% while these final steps are pending.
func (p *transferProgress) finish(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended {
		return
	}
	p.ended = true
	if err == nil {
		p.done = p.total
	}
	if p.enabled {
		p.renderLocked(true, err != nil)
		fmt.Fprint(p.output, "\r\n")
	}
}

func (p *transferProgress) renderLocked(final, failed bool) {
	percent := 0
	if p.total > 0 {
		percent = min(int(float64(p.done)/float64(p.total)*100), 99)
	}
	suffix := ""
	if final {
		if failed {
			suffix = " failed"
		} else {
			percent = 100
		}
	} else if p.done == p.total && p.total > 0 {
		suffix = " finishing..."
	}
	const width = 24
	filled := width * percent / 100
	fmt.Fprintf(p.output, "\r%s [%s%s] %3d%% %s/%s%s\x1b[K", p.label, strings.Repeat("#", filled), strings.Repeat(" ", width-filled), percent, transferBytes(p.done), transferBytes(p.total), suffix)
}

func transferBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(n)
	for _, unit := range units {
		value /= 1024
		if value < 1024 || unit == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%d B", n)
}

type progressReaderAt struct {
	reader   io.ReaderAt
	progress *transferProgress
}

func (r progressReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.reader.ReadAt(p, off)
	r.progress.add(n)
	return n, err
}

type progressWriter struct {
	writer   io.Writer
	progress *transferProgress
}

func (w progressWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.progress.add(n)
	return n, err
}
