package oci

import (
	"fmt"
	"io"
)

// ProgressWriter writes docker-like step progress output.
type ProgressWriter struct {
	w       io.Writer
	total   int
	current int
}

// NewProgressWriter creates a progress writer.
// total is the number of steps that will be reported.
func NewProgressWriter(w io.Writer, total int) *ProgressWriter {
	return &ProgressWriter{w: w, total: total}
}

// Step prints "Step N/M : description" and advances the counter.
func (pw *ProgressWriter) Step(description string) {
	pw.current++
	_, _ = fmt.Fprintf(pw.w, "Step %d/%d : %s\n", pw.current, pw.total, description)
}

// Detail prints " ---> detail" as a sub-step detail line.
func (pw *ProgressWriter) Detail(detail string) {
	_, _ = fmt.Fprintf(pw.w, " ---> %s\n", detail)
}
