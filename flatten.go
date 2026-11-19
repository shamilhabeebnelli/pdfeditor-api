// flatten.go — image-flatten export: every page is re-rendered to a PNG with
// poppler's pdftoppm and rebuilt into a brand-new PDF. The original page
// content, annotations, attachments and metadata do not carry over; text
// becomes part of the page image and is no longer selectable.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/jung-kurt/gofpdf"
	"rsc.io/pdf"
)

// flatten renders every page at dpi and rebuilds the PDF from the images.
// It needs the pdftoppm binary (poppler); when it is missing the caller gets
// an honest error, never a silently different result.
func flatten(pdfBytes []byte, dpi int, pdftoppmPath string) ([]byte, error) {
	if dpi < 72 || dpi > 300 {
		return nil, fmt.Errorf("dpi must be between 72 and 300")
	}
	if _, err := exec.LookPath(pdftoppmPath); err != nil {
		return nil, fmt.Errorf("flatten needs the pdftoppm tool (poppler-utils) installed in the environment; it was not found")
	}

	f, err := pdf.NewReader(bytes.NewReader(pdfBytes), int64(len(pdfBytes)))
	if err != nil {
		return nil, fmt.Errorf("not a readable PDF: %v", err)
	}
	n := f.NumPage()
	if n == 0 {
		return nil, fmt.Errorf("PDF has no pages")
	}
	if n > 200 {
		return nil, fmt.Errorf("flatten is limited to 200 pages per request; this file has %d", n)
	}
	dims, _ := pageSizes(f)

	dir, err := os.MkdirTemp("", "pdfdesk-flatten-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	inPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(inPath, pdfBytes, 0o600); err != nil {
		return nil, err
	}
	outPrefix := filepath.Join(dir, "page")
	cmd := exec.Command(pdftoppmPath, "-png", "-r", fmt.Sprint(dpi), inPath, outPrefix)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("pdftoppm failed: %v (%s)", err, bytes.TrimSpace(combined))
	}
	pages, err := filepath.Glob(outPrefix + "-*.png")
	if err != nil || len(pages) == 0 {
		return nil, fmt.Errorf("pdftoppm produced no page images")
	}
	sort.Strings(pages)
	if len(pages) != n {
		return nil, fmt.Errorf("pdftoppm rendered %d of %d pages; refusing a partial flatten", len(pages), n)
	}

	p := gofpdf.NewCustom(&gofpdf.InitType{UnitStr: "pt", Size: gofpdf.SizeType{Wd: dims[0].W, Ht: dims[0].H}})
	p.SetMargins(0, 0, 0)
	p.SetAutoPageBreak(false, 0)
	for i, png := range pages {
		d := dims[i]
		p.AddPageFormat("P", gofpdf.SizeType{Wd: d.W, Ht: d.H})
		p.ImageOptions(png, 0, 0, d.W, d.H, false, gofpdf.ImageOptions{ImageType: "PNG", ReadDpi: false}, 0, "")
	}
	var buf bytes.Buffer
	if err := p.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
