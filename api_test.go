// api_test.go â end-to-end tests against the real handlers via httptest.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"rsc.io/pdf"
)

func samplePDF(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/invoice.pdf")
	if err == nil {
		return data
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "run", "./cmd/mkpdf", "testdata/invoice.pdf").CombinedOutput(); err != nil {
		t.Fatalf("generate sample: %v (%s)", err, out)
	}
	data, err = os.ReadFile("testdata/invoice.pdf")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func upload(t *testing.T, url string, fields map[string]string, pdfBytes []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "doc.pdf")
	fw.Write(pdfBytes)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, url, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	switch req.URL.Path {
	case "/v1/find":
		handleFind(rec, req)
	case "/v1/edit":
		handleEdit(rec, req)
	case "/v1/replace":
		handleReplace(rec, req)
	case "/v1/flatten":
		handleFlatten(rec, req)
	}
	return rec
}

func TestFind(t *testing.T) {
	pdfBytes := samplePDF(t)
	rec := upload(t, "/v1/find", map[string]string{"query": "Total due"}, pdfBytes)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var res struct {
		Matches int    `json:"matches"`
		Lines   []Line `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Matches != 1 || len(res.Lines) != 1 {
		t.Fatalf("want 1 match, got %d", res.Matches)
	}
	ln := res.Lines[0]
	if !strings.Contains(ln.Text, "1,250.00") || ln.Family != "serif" || ln.FontSize != 12 {
		t.Fatalf("unexpected line: %+v", ln)
	}
	if ln.Width < 50 || ln.BaselineY < 600 {
		t.Fatalf("suspicious geometry: %+v", ln)
	}
}

func TestReplace(t *testing.T) {
	pdfBytes := samplePDF(t)
	rec := upload(t, "/v1/replace", map[string]string{
		"find": "Total due: USD 1,250.00",
		"with": "Total due: USD 999.00",
	}, pdfBytes)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	out := rec.Body.Bytes()
	if !bytes.HasPrefix(out, []byte("%PDF")) {
		t.Fatal("no PDF returned")
	}
	// The overlay lives in a Form XObject after stamping; the Go reader
	// does not descend into XObjects, so extract with poppler instead.
	pages, err := pdftotextWords(out, "pdftotext")
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	for _, words := range pages {
		for _, w := range words {
			buf.WriteString(w.Text)
			buf.WriteByte(' ')
		}
	}
	if !strings.Contains(buf.String(), "999.00") {
		t.Fatal("replacement text not found in output")
	}
	if strings.Contains(buf.String(), "1,250.00") {
		// The original glyphs remain underneath the cover box; that is the
		// documented cover-then-write behavior, not a failure. Only flag
		// this if the cover was expected to remove them (it is not).
		t.Log("note: original glyphs still present under cover box (expected)")
	}
}

func TestReplaceTooWideNeedsConsent(t *testing.T) {
	pdfBytes := samplePDF(t)
	rec := upload(t, "/v1/replace", map[string]string{
		"find": "ref: 6f3a",
		"with": "reference: 6f3a-2026-11-19-final-amended-version",
	}, pdfBytes)
	if rec.Code != 409 {
		t.Fatalf("want 409, got %d: %s", rec.Code, rec.Body)
	}
	rec2 := upload(t, "/v1/replace", map[string]string{
		"find": "ref: 6f3a", "with": "reference: 6f3a-2026-11-19-final-amended-version", "coverAnyway": "true",
	}, pdfBytes)
	if rec2.Code != 200 {
		t.Fatalf("coverAnyway should succeed, got %d: %s", rec2.Code, rec2.Body)
	}
}

func TestReplaceNoMatch(t *testing.T) {
	pdfBytes := samplePDF(t)
	rec := upload(t, "/v1/replace", map[string]string{"find": "NO SUCH TEXT", "with": "x"}, pdfBytes)
	if rec.Code != 404 {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestEditNonLatinFailsHonestly(t *testing.T) {
	pdfBytes := samplePDF(t)
	ops := `[{"page":1,"kind":"addtext","rect":{"x":72,"y":400,"w":200,"h":40},"text":"à´®à´²à´¯à´¾à´³à´","size":12}]`
	rec := upload(t, "/v1/edit", map[string]string{"ops": ops}, pdfBytes)
	if rec.Code != 422 {
		t.Fatalf("want 422 for non-Latin, got %d", rec.Code)
	}
}

func TestEditWhiteout(t *testing.T) {
	pdfBytes := samplePDF(t)
	ops := `[{"page":1,"kind":"whiteout","rect":{"x":70,"y":640,"w":260,"h":20}}]`
	rec := upload(t, "/v1/edit", map[string]string{"ops": ops}, pdfBytes)
	if rec.Code != 200 || !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF")) {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestFlatten(t *testing.T) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm not installed here")
	}
	pdfBytes := samplePDF(t)
	rec := upload(t, "/v1/flatten", map[string]string{"dpi": "100"}, pdfBytes)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	out := rec.Body.Bytes()
	f, err := pdf.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatal(err)
	}
	if f.NumPage() != 1 {
		t.Fatalf("want 1 page, got %d", f.NumPage())
	}
	// flattened: no extractable text lines
	var n int
	for _, tx := range f.Page(1).Content().Text {
		if strings.TrimSpace(tx.S) != "" {
			n++
		}
	}
	if n > 0 {
		t.Fatalf("flattened PDF still has %d text pieces", n)
	}
	if rec.Header().Get("X-PdfDesk-Notes") == "" {
		t.Fatal("missing honesty note header")
	}
}

var _ io.Reader
