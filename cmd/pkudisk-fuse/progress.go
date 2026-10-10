package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/schollz/progressbar/v3"
	"golang.org/x/term"
	"pkudisk-fuse/internal/transfer"
)

type cliProgress struct {
	label  string
	output io.Writer
	bar    *progressbar.ProgressBar
}

func newCLIProgress(direction string, output io.Writer) *cliProgress {
	return &cliProgress{label: strings.ToUpper(direction[:1]) + direction[1:], output: output}
}
func (p *cliProgress) Update(state transfer.Progress) {
	file, ok := p.output.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return
	}
	if p.bar == nil {
		// Reserve the last step for publication/file installation. This keeps
		// the displayed percentage below 100% until the operation succeeds.
		p.bar = progressbar.NewOptions64(state.Total+1,
			progressbar.OptionSetWriter(p.output), progressbar.OptionSetWidth(24),
			progressbar.OptionShowBytes(true), progressbar.OptionShowTotalBytes(false),
			progressbar.OptionUseIECUnits(true), progressbar.OptionSetPredictTime(true),
			progressbar.OptionThrottle(100*time.Millisecond), progressbar.OptionUseANSICodes(true),
			progressbar.OptionOnCompletion(func() { fmt.Fprint(p.output, "\r\n") }),
		)
	}
	phase := ""
	if state.Phase == transfer.Finalizing {
		phase = " finishing..."
	}
	p.bar.Describe(fmt.Sprintf("%s %s/%s%s", p.label, transferBytes(state.Done), transferBytes(state.Total), phase))
	if state.Phase == transfer.Complete {
		_ = p.bar.Finish()
	} else {
		_ = p.bar.Set64(state.Done)
	}
}
func (p *cliProgress) Finish(err error) {
	if p.bar == nil {
		return
	}
	if err != nil {
		p.bar.Describe(p.label + " failed")
		_ = p.bar.Exit()
	}
}
func transferBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= 1024
		if value < 1024 || unit == "TiB" {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%d B", n)
}
