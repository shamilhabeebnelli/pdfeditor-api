// edit.go — cover-and-replace patching: per-page overlay PDFs stamped on top
// of the original with pdfcpu. The original page content stays in the file
// underneath the opaque patches (same semantics as the website's normal
// export); /v1/flatten is the way to make covered content unrecoverable.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/jung-kurt/gofpdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"rsc.io/pdf"
)

// Rect is a rectangle in PDF points, bottom-left origin.
type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Color is an RGB triple, 0..1 per channel. Zero value is black.
type Color struct {
	R float64 `json:"r"`
	G float64 `json:"g"`
	B float64 `json:"b"`
}

// Op is one patch operation, same model as the website's editor-core.
type Op struct {
	Page      int    `json:"page"` // 1-based
	Kind      string `json:"kind"` // whiteout | text | addtext
	Rect      Rect   `json:"rect"`
	Text      string `json:"text"`
	Family    string `json:"family"` // serif | sans | mono (default sans)
	Bold      bool   `json:"bold"`
	Italic    bool   `json:"italic"`
	Underline bool   `json:"underline"`
	Strike    bool   `json:"strike"`
	Size      float64 `json:"size"`
	BaselineY float64 `json:"baselineY"` // text/addtext; default derived from rect
	Color     Color  `json:"color"`
}

// applyOps patches the document and returns the new PDF plus per-op failures
// (ops that cannot be honoured honestly, e.g. non-Latin text).
func applyOps(pdfBytes []byte, ops []Op) ([]byte, []string, error) {
	if len(ops) == 0 {
		return nil, nil, fmt.Errorf("no ops supplied")
	}
	f, err := pdf.NewReader(bytes.NewReader(pdfBytes), int64(len(pdfBytes)))
	if err != nil {
		return nil, nil, fmt.Errorf("not a readable PDF: %v", err)
	}
	dims, warn := pageSizes(f)
	_ = warn

	byPage := map[int][]Op{}
	var failures []string
	for i, op := range ops {
		label := fmt.Sprintf("op %d (page %d %s)", i+1, op.Page, op.Kind)
		if op.Page < 1 || op.Page > f.NumPage() {
			failures = append(failures, label+": no such page")
			continue
		}
		switch op.Kind {
		case "whiteout":
		case "text", "addtext":
			if strings.TrimSpace(op.Text) == "" {
				failures = append(failures, label+": empty text")
				continue
			}
			if bad, ok := winAnsiOK(op.Text); !ok {
				failures = append(failures, fmt.Sprintf("%s: character %q cannot be drawn by the PDF standard fonts (Latin script only in v1); use /v1/flatten output for other scripts", label, bad))
				continue
			}
			if op.Size <= 0 {
				op.Size = 12
			}
		default:
			failures = append(failures, label+": kind must be whiteout, text or addtext")
			continue
		}
		if op.Kind == "whiteout" || op.Kind == "text" {
			if op.Rect.W <= 0 || op.Rect.H <= 0 {
				failures = append(failures, label+": rect needs positive w and h")
				continue
			}
		}
		byPage[op.Page] = append(byPage[op.Page], op)
	}
	if len(byPage) == 0 {
		return nil, failures, fmt.Errorf("no usable ops")
	}

	conf, err := api.LoadConfiguration(api.ConfigurationOptions{})
	if err != nil {
		return nil, failures, fmt.Errorf("pdfcpu configuration: %v", err)
	}
	conf.Cmd = 0

	ctx := context.Background()
	wms := map[int]*model.Watermark{}
	for page, pageOps := range byPage {
		ov, err := buildOverlay(dims[page-1], pageOps)
		if err != nil {
			return nil, failures, fmt.Errorf("page %d overlay: %v", page, err)
		}
		wm, err := api.PDFWatermarkForReadSeeker(ctx, bytes.NewReader(ov), 1,
			"pos:full, scalefactor:1 abs, rotation:0, opacity:1", true, false, types.POINTS)
		if err != nil {
			return nil, failures, fmt.Errorf("page %d watermark: %v", page, err)
		}
		wms[page] = wm
	}

	var out bytes.Buffer
	err = api.AddWatermarksMap(ctx, bytesReader{bytes.NewReader(pdfBytes)}, &out, wms, conf)
	if err != nil {
		return nil, failures, fmt.Errorf("stamping failed: %v", err)
	}
	return out.Bytes(), failures, nil
}

// buildOverlay renders one transparent page carrying only the patches.
func buildOverlay(d pageDims, ops []Op) ([]byte, error) {
	p := gofpdf.NewCustom(&gofpdf.InitType{UnitStr: "pt", Size: gofpdf.SizeType{Wd: d.W, Ht: d.H}})
	p.SetMargins(0, 0, 0)
	p.SetAutoPageBreak(false, 0)
	p.AddPage()
	for _, op := range ops {
		switch op.Kind {
		case "whiteout":
			p.SetFillColor(255, 255, 255)
			p.Rect(op.Rect.X, d.H-op.Rect.Y-op.Rect.H, op.Rect.W, op.Rect.H, "F")
		case "text":
			p.SetFillColor(255, 255, 255)
			p.Rect(op.Rect.X, d.H-op.Rect.Y-op.Rect.H, op.Rect.W, op.Rect.H, "F")
			drawTextLines(p, d, op, op.Text, op.BaselineYOr())
		case "addtext":
			drawTextLines(p, d, op, op.Text, op.BaselineYOr())
		}
	}
	var buf bytes.Buffer
	if err := p.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// BaselineYOr returns the op's baseline or a sensible default inside the rect.
func (op Op) BaselineYOr() float64 {
	if op.BaselineY > 0 {
		return op.BaselineY
	}
	return op.Rect.Y + op.Rect.H*0.72
}

// drawTextLines draws one or more lines (LF-separated) with family styling
// and underline/strikethrough rules. First line sits on baselineY; further
// lines go below it. A leading "- " becomes a bullet, like the website.
func drawTextLines(p *gofpdf.Fpdf, d pageDims, op Op, text string, firstBaseline float64) {
	fam, style := goFontName(op.Family, op.Bold, op.Italic)
	p.SetFont(fam, style, op.Size)
	p.SetTextColor(int(op.Color.R*255), int(op.Color.G*255), int(op.Color.B*255))
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(ln, "- ") {
			ln = "\x95 " + ln[2:] // cp1252 bullet
		}
		base := firstBaseline - float64(i)*op.Size*1.25
		yTopDown := d.H - base
		// go-fpdf Text() places the baseline at the given y.
		p.Text(op.Rect.X, yTopDown, cp1252(ln))
		w := p.GetStringWidth(cp1252(ln))
		if op.Underline {
			p.SetDrawColor(int(op.Color.R*255), int(op.Color.G*255), int(op.Color.B*255))
			p.SetLineWidth(op.Size * 0.05)
			p.Line(op.Rect.X, yTopDown+op.Size*0.15, op.Rect.X+w, yTopDown+op.Size*0.15)
		}
		if op.Strike {
			p.SetDrawColor(int(op.Color.R*255), int(op.Color.G*255), int(op.Color.B*255))
			p.SetLineWidth(op.Size * 0.05)
			p.Line(op.Rect.X, yTopDown-op.Size*0.3, op.Rect.X+w, yTopDown-op.Size*0.3)
		}
	}
}

// cp1252 maps the small set of supported non-Latin-1 cp1252 runes to their
// byte values for go-fpdf's core fonts.
func cp1252(s string) string {
	m := map[rune]byte{
		'€': 0x80, '‚': 0x82, 'ƒ': 0x83, '„': 0x84, '…': 0x85, '†': 0x86,
		'‡': 0x87, 'ˆ': 0x88, '‰': 0x89, 'Š': 0x8A, '‹': 0x8B, 'Œ': 0x8C,
		'Ž': 0x8E, '‘': 0x91, '’': 0x92, '“': 0x93, '”': 0x94, '•': 0x95,
		'–': 0x96, '—': 0x97, '˜': 0x98, '™': 0x99, 'š': 0x9A, '›': 0x9B,
		'œ': 0x9C, 'ž': 0x9E, 'Ÿ': 0x9F,
	}
	var b strings.Builder
	for _, r := range s {
		if r < 256 {
			b.WriteByte(byte(r))
		} else if c, ok := m[r]; ok {
			b.WriteByte(c)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// bytesReader adapts a *bytes.Reader to io.ReadSeeker (for clarity at call sites).
type bytesReader struct{ *bytes.Reader }

var _ io.ReadSeeker = bytesReader{}
