package main

import (
	"fmt"
	"io"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
)

// Lip Gloss measures terminal cell widths, including CJK and emoji. Keeping one
// borderless renderer gives listing commands consistent spacing without custom
// Unicode padding or ANSI/color output when redirected.
func printTable(output io.Writer, headers []string, rows [][]string, numbers ...int) error {
	for _, row := range rows {
		for i, cell := range row {
			row[i] = tuiText(cell)
		}
	}
	rendered := table.New().Headers(headers...).Rows(rows...).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderHeader(false).BorderColumn(false).Wrap(false).
		StyleFunc(func(_, col int) lipgloss.Style {
			style := lipgloss.NewStyle()
			if col < len(headers)-1 {
				style = style.PaddingRight(2)
			}
			for _, number := range numbers {
				if col == number {
					return style.Align(lipgloss.Right)
				}
			}
			return style
		}).String()
	_, err := fmt.Fprintln(output, rendered)
	return err
}
