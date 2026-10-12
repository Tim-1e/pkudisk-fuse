package main

import (
	"fmt"
	"strconv"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

type sizeFormat struct{ bytes bool }

func (s *sizeFormat) add(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&s.bytes, "bytes", "b", false, "show exact byte counts instead of readable IEC units; JSON stays numeric")
}
func (s sizeFormat) value(n int64) string {
	if n < 0 {
		return "-"
	}
	if s.bytes {
		return strconv.FormatInt(n, 10)
	}
	return humanize.IBytes(uint64(n))
}
func (s sizeFormat) label(n int64) string {
	if s.bytes {
		return fmt.Sprintf("%d bytes", n)
	}
	return s.value(n)
}
