// main.go â PDF Desk self-host API: an HTTP wrapper around the same editing
// semantics as https://pdfeditor.denelli.in, for developers to run next to
// their own bots and apps. No account, no key, no hosted endpoint.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"rsc.io/pdf"
)

const maxUploadBytes = 25 << 20 // 25 MB, same bound as the PhotoFixer API

var processing = make(chan struct{}, 4) // bounded concurrency: image/PDF work is CPU+RAM heavy

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/v1/find", handleFind)
	mux.HandleFunc("/v1/edit", handleEdit)
	mux.HandleFunc("/v1/replace", handleReplace)
	mux.HandleFunc("/v1/flatten", handleFlatten)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Println("pdfdesk-api listening on :" + port)
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "PDF Desk self-host API (private build - unpublished).\n"+
		"POST /v1/find     multipart: file + optional query, page -> positioned text lines (JSON)\n"+
		"POST /v1/edit     multipart: file + ops (JSON) -> patched PDF\n"+
		"POST /v1/replace  multipart: file + find + with (+ optional page, all, coverAnyway) -> patched PDF\n"+
		"POST /v1/flatten  multipart: file + optional dpi -> image-flattened PDF\n"+
		"GET  /healthz\n")
}

func problem(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// readUpload validates and reads the multipart "file" field.
func readUpload(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Method != http.MethodPost {
		problem(w, 405, "POST only.")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		problem(w, 400, "Send the PDF as multipart/form-data in a 'file' field (max 25 MB).")
		return nil, false
	}
	fh, _, err := r.FormFile("file")
	if err != nil {
		problem(w, 400, "Attach the PDF as form field 'file'.")
		return nil, false
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, maxUploadBytes+1))
	if err != nil || len(data) == 0 {
		problem(w, 400, "The uploaded file could not be read.")
		return nil, false
	}
	if len(data) > maxUploadBytes {
		problem(w, 400, "PDF is larger than the 25 MB upload limit.")
		return nil, false
	}
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		problem(w, 415, "Not a PDF file.")
		return nil, false
	}
	return data, true
}

// gate bounds concurrent processing; honest 503 when busy.
func gate(w http.ResponseWriter) bool {
	select {
	case processing <- struct{}{}:
		return true
	case <-time.After(15 * time.Second):
		problem(w, 503, "Busy processing other documents right now; retry in a few seconds.")
		return false
	}
}

func ungate() { <-processing }

func openPDF(data []byte) (*pdf.Reader, error) {
	return pdf.NewReader(bytes.NewReader(data), int64(len(data)))
}

func handleFind(w http.ResponseWriter, r *http.Request) {
	data, ok := readUpload(w, r)
	if !ok {
		return
	}
	if !gate(w) {
		return
	}
	defer ungate()

	f, err := openPDF(data)
	if err != nil {
		problem(w, 422, "Not a readable PDF: "+err.Error())
		return
	}
	query := r.FormValue("query")
	page := formInt(r, "page", 0)
	if page < 0 || page > f.NumPage() {
		problem(w, 400, fmt.Sprintf("page must be between 1 and %d.", f.NumPage()))
		return
	}
	all, warnings, err := extractLines(data, pdftotextTool())
	if err != nil {
		problem(w, 422, err.Error())
		return
	}
	matches := matchLines(all, query, page)
	if matches == nil {
		matches = []Line{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"pages":    f.NumPage(),
		"query":    query,
		"matches":  len(matches),
		"lines":    matches,
		"warnings": warnings,
		"space":    "coordinates are PDF points, origin at the bottom-left of each page",
	})
}

func handleEdit(w http.ResponseWriter, r *http.Request) {
	data, ok := readUpload(w, r)
	if !ok {
		return
	}
	var ops []Op
	if raw := r.FormValue("ops"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &ops); err != nil {
			problem(w, 400, "ops must be a JSON array of operations: "+err.Error())
			return
		}
	}
	if len(ops) == 0 {
		problem(w, 400, "Pass at least one operation in the 'ops' form field (JSON array).")
		return
	}
	if !gate(w) {
		return
	}
	defer ungate()

	out, failures, err := applyOps(data, ops)
	if err != nil {
		problem(w, 422, err.Error())
		return
	}
	writePDF(w, out, "pdfdesk-edited.pdf", failures,
		"Cover-and-replace: original content stays in the file under opaque patches; use /v1/flatten when covered content must be unrecoverable. EXIF/metadata of the original file are preserved as-is.")
}

func handleReplace(w http.ResponseWriter, r *http.Request) {
	data, ok := readUpload(w, r)
	if !ok {
		return
	}
	find := r.FormValue("find")
	with := r.FormValue("with")
	if find == "" {
		problem(w, 400, "Pass the text to find in the 'find' form field.")
		return
	}
	if bad, okk := winAnsiOK(with); !okk {
		problem(w, 400, fmt.Sprintf("Replacement character %q cannot be drawn by the PDF standard fonts (Latin script only in v1).", bad))
		return
	}
	page := formInt(r, "page", 0)
	all := r.FormValue("all") == "true"
	coverAnyway := r.FormValue("coverAnyway") == "true"

	if !gate(w) {
		return
	}
	defer ungate()

	if _, err := openPDF(data); err != nil {
		problem(w, 422, "Not a readable PDF: "+err.Error())
		return
	}
	lines, _, err := extractLines(data, pdftotextTool())
	if err != nil {
		problem(w, 422, err.Error())
		return
	}
	matches := matchLines(lines, find, page)
	if len(matches) == 0 {
		problem(w, 404, "No matching text line found. Note: scanned (image-only) pages have no text layer to search.")
		return
	}
	if !all {
		matches = matches[:1]
	}

	// Width honesty, same rule as the website: a wider replacement covers
	// content to its right; the caller must opt in explicitly.
	var ops []Op
	var tooWide []string
	for _, m := range matches {
		newW := stringWidth(with, m.Family, m.Bold, m.Italic, m.FontSize)
		rect := Rect{X: m.X, Y: m.BaselineY - m.FontSize*0.28, W: m.Width, H: m.FontSize * 1.36}
		if newW > m.Width+1 {
			if !coverAnyway {
				tooWide = append(tooWide, fmt.Sprintf("page %d: replacement needs %.0f pt but the original line is %.0f pt wide", m.Page, newW, m.Width))
				continue
			}
			rect.W = newW + 2
		}
		ops = append(ops, Op{
			Page: m.Page, Kind: "text", Rect: rect, Text: with,
			Family: m.Family, Bold: m.Bold, Italic: m.Italic,
			Size: m.FontSize, BaselineY: m.BaselineY,
		})
	}
	if len(ops) == 0 {
		problem(w, 409, "Replacement is wider than the original text and would cover content to its right. Resend with coverAnyway=true to accept that. "+strings.Join(tooWide, "; "))
		return
	}

	out, failures, err := applyOps(data, ops)
	if err != nil {
		problem(w, 422, err.Error())
		return
	}
	notes := fmt.Sprintf("Replaced %d line(s). Cover-and-replace: the original text stays in the file under an opaque patch and can still be extractable; use /v1/flatten when it must be unrecoverable.", len(ops))
	writePDF(w, out, "pdfdesk-replaced.pdf", failures, notes)
}

func handleFlatten(w http.ResponseWriter, r *http.Request) {
	data, ok := readUpload(w, r)
	if !ok {
		return
	}
	dpi := formInt(r, "dpi", 150)
	if !gate(w) {
		return
	}
	defer ungate()

	tool := os.Getenv("PDFDESK_PDFTOPPM")
	if tool == "" {
		tool = "pdftoppm"
	}
	out, err := flatten(data, dpi, tool)
	if err != nil {
		status := 422
		if strings.Contains(err.Error(), "not found") {
			status = 501
		}
		problem(w, status, err.Error())
		return
	}
	writePDF(w, out, "pdfdesk-flattened.pdf", nil,
		fmt.Sprintf("Pages re-rendered as images at %d dpi and rebuilt into a brand-new PDF; original page content, annotations, attachments and metadata do not carry over; text is no longer selectable.", dpi))
}

func writePDF(w http.ResponseWriter, data []byte, name string, failures []string, notes string) {
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	w.Header().Set("X-PdfDesk-Bytes", fmt.Sprint(len(data)))
	if notes != "" {
		w.Header().Set("X-PdfDesk-Notes", notes)
	}
	if len(failures) > 0 {
		w.Header().Set("X-PdfDesk-Failures", strings.Join(failures, " | "))
	}
	w.Write(data)
}

func formInt(r *http.Request, key string, def int) int {
	v := r.FormValue(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}

func pdftotextTool() string {
	if t := os.Getenv("PDFDESK_PDFTOTEXT"); t != "" {
		return t
	}
	return "pdftotext"
}

// stringWidth measures replacement text with the same core fonts go-fpdf draws.
func stringWidth(s, family string, bold, italic bool, size float64) float64 {
	widthMu.Lock()
	defer widthMu.Unlock()
	if measurePDF == nil {
		measurePDF = gofpdfMeasure()
	}
	return measurePDF(s, family, bold, italic, size)
}

var (
	widthMu    sync.Mutex
	measurePDF func(s, family string, bold, italic bool, size float64) float64
)
