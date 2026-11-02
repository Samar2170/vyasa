package ir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ledongthuc/pdf"
)

// errNotPDF marks a download that answered with something other than a PDF
// (usually an HTML listing page); callers use it as the signal to look one
// level deeper for document links on that page.
var errNotPDF = errors.New("response is not a PDF")

// download fetches a filing PDF with browser headers (IR sites commonly
// require a Referer) and verifies it really is a PDF.
func (c *Client) download(ctx context.Context, u, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.setBrowseHeaders(req, referer)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ir: %s: %s", u, resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDocBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDocBytes {
		return nil, fmt.Errorf("ir: %s: file exceeds %d bytes", u, maxDocBytes)
	}
	if len(data) < 1024 || !bytes.Contains(data[:1024], []byte("%PDF")) {
		return nil, fmt.Errorf("ir: %s: %w", u, errNotPDF)
	}
	return data, nil
}

// extractPDFText renders the plain text of every page, tolerating individual
// page failures. Returns the text and the number of readable pages.
func extractPDFText(data []byte) (string, int) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", 0
	}

	var b strings.Builder
	readable := 0
	for i := 1; i <= r.NumPage(); i++ {
		page := r.Page(i)
		txt, err := safePageText(page)
		if err != nil {
			continue
		}
		readable++
		b.WriteString(strings.TrimRight(txt, " \t\n"))
		b.WriteString("\n")
	}
	return collapseBlankLines(b.String()), readable
}

// safePageText guards against panics inside the pdf parser on malformed
// pages.
func safePageText(p pdf.Page) (s string, err error) {
	defer func() {
		if recover() != nil {
			s, err = "", fmt.Errorf("ir: unrecoverable page")
		}
	}()
	return p.GetPlainText(nil)
}

// collapseBlankLines trims whitespace and squeezes 3+ newlines to two.
func collapseBlankLines(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	blank := 0
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// ExtractPDFText is the exported form of extractPDFText for packages outside
// ir (e.g. the agent web tools) that also need to read archived PDFs.
func ExtractPDFText(data []byte) (string, int) {
	return extractPDFText(data)
}
