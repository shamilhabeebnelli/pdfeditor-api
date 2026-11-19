// mkpdf generates the demo/test invoice PDF (deterministic, no external assets).
package main

import (
	"fmt"
	"os"

	"github.com/jung-kurt/gofpdf"
)

func main() {
	out := "invoice.pdf"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	p := gofpdf.New("P", "pt", "A4", "")
	p.AddPage()
	p.SetFont("Helvetica", "B", 18)
	p.SetXY(72, 72)
	p.Cell(0, 22, "INVOICE #1042")
	p.SetFont("Helvetica", "", 11)
	p.SetXY(72, 104)
	p.Cell(0, 14, "Denelli Demo Stores, Kozhikode")
	p.SetFont("Times", "", 12)
	p.SetXY(72, 140)
	p.Cell(0, 15, "Item: Website care plan (1 year)")
	p.SetXY(72, 162)
	p.Cell(0, 15, "Total due: USD 1,250.00")
	p.SetFont("Courier", "", 10)
	p.SetXY(72, 196)
	p.Cell(0, 13, "ref: 6f3a-2026-11-19")
	p.SetFont("Times", "I", 10)
	p.SetXY(72, 740)
	p.Cell(0, 12, "Thank you for your business.")
	if err := p.OutputFileAndClose(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("wrote", out)
}
