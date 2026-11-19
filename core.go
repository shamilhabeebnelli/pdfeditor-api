// core.go — shared PDF Desk logic: text-line discovery, font mapping and the
// op model. Pure Go; the same semantics as the browser editor
// (pdfeditor.denelli.in) but with no JavaScript and no browser.
//
// Op model. All geometry is in PDF user space (points, bottom-left origin),
// the same convention the website's editor-core.mjs documents.
//
// Text discovery is a hybrid of two readers, each covering the other's blind
// spot: poppler's pdftotext -bbox gives exact word boxes with real spaces,
// and the Go PDF reader gives the font name, size and exact baseline for
// those words. A page with no text layer (a scan) simply yields no lines.
package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"rsc.io/pdf"
)

// A Line is one discovered text line with its position in PDF points.
type Line struct {
	Page      int     `json:"page"` // 1-based
	X         float64 `json:"x"`
	Y         float64 `json:"y"` // bottom of the line box, bottom-up points
	Width     float64 `json:"width"`
	Height    float64 `json:"height"`
	BaselineY float64 `json:"baselineY"`
	Font      string  `json:"font"` // original font resource name ("" when unknown)
	Family    string  `json:"family"`
	Bold      bool    `json:"bold"`
	Italic    bool    `json:"italic"`
	FontSize  float64 `json:"fontSize"`
	Text      string  `json:"text"`
}

// mapFontFamily mirrors editor-core.mjs: subset prefix stripped, weight/slant
// detected from the font name, family matched by keyword.
func mapFontFamily(originalFontName string) (family string, bold, italic bool) {
	name := originalFontName
	if len(name) > 7 && name[6] == '+' {
		name = name[7:] // strip "ABCDEF+" subset prefix
	}
	lower := strings.ToLower(name)
	bold = strings.Contains(lower, "bold") || strings.Contains(lower, "black") ||
		strings.Contains(lower, "heavy") || strings.Contains(lower, "semibold") || strings.Contains(lower, "demi")
	italic = strings.Contains(lower, "italic") || strings.Contains(lower, "oblique") || strings.Contains(lower, "slant")
	family = "sans"
	for _, kw := range []string{"times", "roman", "serif", "georgia", "palatino", "book"} {
		if strings.Contains(lower, kw) {
			family = "serif"
			break
		}
	}
	if family == "sans" {
		for _, kw := range []string{"courier", "mono", "typewriter", "consol"} {
			if strings.Contains(lower, kw) {
				family = "mono"
				break
			}
		}
	}
	return family, bold, italic
}

// goFontName maps family+style to a go-fpdf core font family and style string.
func goFontName(family string, bold, italic bool) (string, string) {
	style := ""
	if bold {
		style += "B"
	}
	if italic {
		style += "I"
	}
	switch family {
	case "serif":
		return "Times", style
	case "mono":
		return "Courier", style
	default:
		return "Helvetica", style
	}
}

// mediaBox walks the page -> parent chain for an inherited MediaBox.
func mediaBox(pg pdf.Page) (x0, y0, x1, y1 float64, ok bool) {
	v := pg.V
	for !v.IsNull() {
		mb := v.Key("MediaBox")
		if mb.Len() == 4 {
			return mb.Index(0).Float64(), mb.Index(1).Float64(), mb.Index(2).Float64(), mb.Index(3).Float64(), true
		}
		v = v.Key("Parent")
	}
	return 0, 0, 0, 0, false
}

type pageDims struct {
	W, H float64
}

// pageSizes returns the dimensions of every page in points.
func pageSizes(f *pdf.Reader) ([]pageDims, []string) {
	var dims []pageDims
	var warnings []string
	for i := 1; i <= f.NumPage(); i++ {
		pg := f.Page(i)
		if pg.V.IsNull() {
			warnings = append(warnings, fmt.Sprintf("page %d: unreadable page dictionary; treated as A4", i))
			dims = append(dims, pageDims{595.28, 841.89})
			continue
		}
		x0, y0, x1, y1, ok := mediaBox(pg)
		if !ok || x1-x0 < 10 || y1-y0 < 10 {
			warnings = append(warnings, fmt.Sprintf("page %d: no usable MediaBox; treated as A4", i))
			dims = append(dims, pageDims{595.28, 841.89})
			continue
		}
		dims = append(dims, pageDims{x1 - x0, y1 - y0})
	}
	return dims, warnings
}

// --- pdftotext -bbox (word geometry, top-down points) ---

type bboxDoc struct {
	Pages []bboxPage `xml:"page"`
}
type bboxPage struct {
	Width  float64    `xml:"width,attr"`
	Height float64    `xml:"height,attr"`
	Words  []bboxWord `xml:"word"`
}
type bboxWord struct {
	XMin float64 `xml:"xMin,attr"`
	YMin float64 `xml:"yMin,attr"`
	XMax float64 `xml:"xMax,attr"`
	YMax float64 `xml:"yMax,attr"`
	Text string  `xml:",chardata"`
}

// pdftotextWords runs poppler's pdftotext -bbox and returns word boxes per page.
func pdftotextWords(pdfBytes []byte, tool string) ([][]bboxWord, error) {
	cmd := exec.Command(tool, "-bbox", "-", "-")
	cmd.Stdin = bytes.NewReader(pdfBytes)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftotext failed: %v (%s)", err, strings.TrimSpace(errOut.String()))
	}
	// Pages sit nested inside <html><body><doc>, so decode streaming and
	// pick up each <page> wherever it appears.
	dec := xml.NewDecoder(&out)
	var pages [][]bboxWord
	for {
		tok, err := dec.Token()
		if err != nil {
			break // io.EOF or malformed tail; keep what decoded
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "page" {
			var p bboxPage
			if err := dec.DecodeElement(&p, &se); err == nil {
				pages = append(pages, p.Words)
			}
		}
	}
	return pages, nil
}

// --- Go reader font runs (font name, size, exact baseline) ---

type fontRun struct {
	y      float64 // baseline, bottom-up
	x0     float64
	font   string
	size   float64
}

// fontRuns groups the Go reader's glyph items into runs sharing font, size
// and baseline. Only font identity and the baseline are taken from it - the
// reader does not advance the pen for some producers, so geometry comes from
// pdftotext instead.
func fontRuns(pg pdf.Page) (runs []fontRun) {
	defer func() { _ = recover() }()
	for _, it := range pg.Content().Text {
		if strings.TrimSpace(it.S) == "" {
			continue
		}
		if n := len(runs); n > 0 {
			last := &runs[n-1]
			tol := 1.5
			if last.size*0.2 > tol {
				tol = last.size * 0.2
			}
			if last.font == it.Font && last.size == it.FontSize && abs(it.Y-last.y) <= tol {
				continue
			}
		}
		runs = append(runs, fontRun{y: it.Y, x0: it.X, font: it.Font, size: it.FontSize})
	}
	return runs
}

// extractLines discovers positioned text lines on every page. pdftotextPath
// is the poppler pdftotext binary; when it is missing the extractor falls
// back to the Go reader alone and warns that spaces may be lost.
func extractLines(pdfBytes []byte, pdftotextPath string) ([][]Line, []string, error) {
	f, err := pdf.NewReader(bytes.NewReader(pdfBytes), int64(len(pdfBytes)))
	if err != nil {
		return nil, nil, fmt.Errorf("not a readable PDF: %v", err)
	}
	dims, warnings := pageSizes(f)

	wordPages, werr := pdftotextWords(pdfBytes, pdftotextPath)
	if werr != nil {
		warnings = append(warnings, "pdftotext unavailable ("+werr.Error()+"); fell back to the built-in reader, in which spaces between words can be lost")
		return extractLinesFallback(f), warnings, nil
	}

	var all [][]Line
	for i := 1; i <= f.NumPage(); i++ {
		pg := f.Page(i)
		var runs []fontRun
		if !pg.V.IsNull() {
			runs = fontRuns(pg)
		}
		var words []bboxWord
		if i-1 < len(wordPages) {
			words = wordPages[i-1]
		}
		all = append(all, wordsToLines(words, runs, dims[i-1], i))
	}
	return all, warnings, nil
}

// wordsToLines groups a page's word boxes into visual lines and attaches
// font identity from the nearest baseline run.
func wordsToLines(words []bboxWord, runs []fontRun, d pageDims, pageNo int) []Line {
	// group by vertical position
	sort.SliceStable(words, func(i, j int) bool {
		if abs(words[i].YMin-words[j].YMin) > 2 {
			return words[i].YMin < words[j].YMin
		}
		return words[i].XMin < words[j].XMin
	})
	var groups [][]bboxWord
	for _, w := range words {
		if n := len(groups); n > 0 {
			last := groups[n-1][0]
			h := last.YMax - last.YMin
			if h < 4 {
				h = 4
			}
			if abs(w.YMin-last.YMin) <= h*0.4 {
				groups[n-1] = append(groups[n-1], w)
				continue
			}
		}
		groups = append(groups, []bboxWord{w})
	}

	var out []Line
	for _, g := range groups {
		sort.SliceStable(g, func(i, j int) bool { return g[i].XMin < g[j].XMin })
		x0 := g[0].XMin
		x1 := g[0].XMax
		var yMax float64
		var parts []string
		for _, w := range g {
			if w.XMax > x1 {
				x1 = w.XMax
			}
			if w.YMax > yMax {
				yMax = w.YMax
			}
			parts = append(parts, strings.TrimSpace(w.Text))
		}
		yb := d.H - yMax // box bottom in bottom-up points

		// nearest font run by baseline (baseline sits ~descent above the box bottom)
		best := -1
		bestDist := 1e9
		for ri, r := range runs {
			dist := abs(r.y - yb)
			if dist < bestDist {
				bestDist = dist
				best = ri
			}
		}
		var fontName, fam string
		var b, it bool
		var size, baseline float64
		if best >= 0 && bestDist <= 6 {
			r := runs[best]
			fontName = r.font
			size = r.size
			baseline = r.y
			fam, b, it = mapFontFamily(r.font)
		} else {
			size = (g[0].YMax - g[0].YMin) / 0.9
			baseline = yb + size*0.22
			fam = "sans"
		}
		out = append(out, Line{
			Page:      pageNo,
			X:         x0,
			Y:         baseline - size*0.28,
			Width:     x1 - x0,
			Height:    size * 1.36,
			BaselineY: baseline,
			Font:      fontName,
			Family:    fam,
			Bold:      b,
			Italic:    it,
			FontSize:  size,
			Text:      strings.Join(parts, " "),
		})
	}
	return out
}

// extractLinesFallback uses only the Go reader when poppler is unavailable.
// Honest limit: word spacing can be lost for some producers.
func extractLinesFallback(f *pdf.Reader) [][]Line {
	var all [][]Line
	for i := 1; i <= f.NumPage(); i++ {
		pg := f.Page(i)
		if pg.V.IsNull() {
			all = append(all, nil)
			continue
		}
		var lines []Line
		for _, r := range fontRuns(pg) {
			fam, b, it := mapFontFamily(r.font)
			lines = append(lines, Line{
				Page: i, X: r.x0, Y: r.y - r.size*0.28, Height: r.size * 1.36,
				BaselineY: r.y, Font: r.font, Family: fam, Bold: b, Italic: it,
				FontSize: r.size, Text: "(text unavailable without pdftotext)",
			})
		}
		all = append(all, lines)
	}
	return all
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// matchLines returns lines whose text contains the query (case-insensitive),
// optionally restricted to one 1-based page.
func matchLines(all [][]Line, query string, page int) []Line {
	q := strings.ToLower(query)
	var out []Line
	for pi, lines := range all {
		if page > 0 && pi+1 != page {
			continue
		}
		for _, ln := range lines {
			if q == "" || strings.Contains(strings.ToLower(ln.Text), q) {
				out = append(out, ln)
			}
		}
	}
	return out
}

// winAnsiOK reports whether every rune can be encoded by the PDF standard
// fonts (WinAnsiEncoding, cp1252). Same honest v1 limit as the website:
// direct edits are Latin-script only; flatten has no such limit.
func winAnsiOK(s string) (bad rune, ok bool) {
	const extra = "€‚ƒ„…†‡ˆ‰Š‹ŒŽ‘’“”•–—˜™š›œžŸ"
	for _, r := range s {
		if r < 256 || strings.ContainsRune(extra, r) {
			continue
		}
		return r, false
	}
	return 0, true
}
