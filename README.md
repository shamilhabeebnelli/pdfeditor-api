# PDF Desk API

Backend HTTP API for programmatic PDF text work, for developers to download
and run in their own projects and bots. Written in Go. Companion to the
PDF Desk browser editor, but fully standalone.

## Get it

Download the ZIP from GitHub (Code -> Download ZIP), or directly:
https://github.com/shamilhabeebnelli/pdfeditor-api/archive/refs/heads/main.zip

## Run it

Docker (recommended, includes poppler):

    docker build -t pdfdesk-api .
    docker run -p 8080:8080 pdfdesk-api

Or with Go 1.26+ and poppler-utils installed (`apt install poppler-utils` /
`brew install poppler`):

    go build -o pdfdesk-api .
    ./pdfdesk-api

`PORT` sets the listen port (default 8080). `PDFDESK_PDFTOTEXT` and
`PDFDESK_PDFTOPPM` override the poppler binary paths.

## Endpoints

All PDF-in endpoints take the document as multipart form field `file`
(max 25 MB).

- `GET /healthz` - liveness.
- `POST /v1/find` - locate text. Form fields: `query` (required), `page`
  (0 = all pages). Returns each matching line with exact coordinates (PDF
  points, bottom-left origin), font name, family, size, bold/italic flags.
  Use it to check a string exists before replacing.
- `POST /v1/replace` - cover a matched line with a white box and write the
  replacement in a matching standard font at the same spot. Fields: `find`,
  `with`, `page` (0 = first match anywhere), `all=true` (every match),
  `coverAnyway=true` (consent when the replacement is wider than the space
  the original occupied - without it you get HTTP 409 instead of a silent
  overlap). Returns the edited PDF.
- `POST /v1/edit` - low-level ops: cover rectangles, add text lines.
- `POST /v1/flatten` - re-render every page to an image and rebuild the PDF
  from those images. The output has no text layer; annotations, attachments,
  metadata and the original file structure do not carry over. This is not a
  secure-redaction feature: it is a visual rebuild, capped at 200 pages.

## Honest limits

- Text search and replace work on real text layers. Scanned (image-only)
  pages have no text to find - `/v1/find` reports zero matches.
- Replacement text is drawn with the PDF standard fonts (Latin script only).
  Requests with characters those fonts cannot draw fail with HTTP 400
  instead of printing garbage.
- Exact original-font fidelity is not promised. The replacement uses the
  closest standard-font match (serif/sans/mono, bold, italic) at the
  original size.
- Replace covers the old glyphs with a white box; the original bytes remain
  in the file's content stream. If the old text must not survive extraction,
  run `/v1/flatten` after editing.
- Poppler (`pdftotext`, `pdftoppm`) is a runtime dependency. Without
  `pdftotext`, find/replace fall back to a Go-only extractor that is less
  precise for some producers (a warning says so); without `pdftoppm`,
  `/v1/flatten` returns HTTP 501.

## Demo

`demo/demo.sh` builds a sample invoice, finds "Total due", replaces the
amount and flattens the result, printing each HTTP status. Run it against a
locally running server.

## Layout

- `main.go` - HTTP server and endpoint handlers
- `core.go` - text extraction (poppler word boxes + Go font runs)
- `edit.go` - cover/replace/add-text ops over pdfcpu + go-fpdf
- `flatten.go` - page re-render rebuild
- `api_test.go` - handler test suite (`go test ./...`)
- `cmd/mkpdf/` - sample invoice generator used by tests and the demo

## License

MIT - see LICENSE. All code in this repository is MIT-licensed and free to
use, modify and redistribute.
