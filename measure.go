// measure.go â string-width measuring with go-fpdf's core font metrics.
package main

import "github.com/jung-kurt/gofpdf"

// gofpdfMeasure returns a width function backed by a scratch go-fpdf document
// (metrics only; nothing is rendered).
func gofpdfMeasure() func(s, family string, bold, italic bool, size float64) float64 {
	p := gofpdf.New("P", "pt", "A4", "")
	p.AddPage()
	return func(s, family string, bold, italic bool, size float64) float64 {
		fam, style := goFontName(family, bold, italic)
		p.SetFont(fam, style, size)
		return p.GetStringWidth(cp1252(s))
	}
}
