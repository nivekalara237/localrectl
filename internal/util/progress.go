package util

import "io"

// ProgressFunc receives the bytes transferred so far and the total, -1 when it is unknown.
type ProgressFunc func(done, total int64)

// ProgressReader counts the bytes read from R and reports them to Report after each read.
type ProgressReader struct {
	R      io.Reader
	Total  int64
	Report ProgressFunc

	done int64
}

// NewProgressReader returns a reader that reports the progress of r to report. total is -1
// when the size is unknown.
func NewProgressReader(r io.Reader, total int64, report ProgressFunc) *ProgressReader {
	return &ProgressReader{R: r, Total: total, Report: report}
}

func (p *ProgressReader) Read(b []byte) (int, error) {
	n, err := p.R.Read(b)
	if n > 0 {
		p.done += int64(n)
		if p.Report != nil {
			p.Report(p.done, p.Total)
		}
	}
	return n, err
}

// Close closes R when it is an io.Closer, so a wrapped response body is still released.
func (p *ProgressReader) Close() error {
	if c, ok := p.R.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
