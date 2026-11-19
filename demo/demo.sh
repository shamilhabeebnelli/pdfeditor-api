#!/bin/sh
# PDF Desk API demo: build a sample invoice, find a line, replace the amount,
# flatten the result. Needs a running server (default http://localhost:8080),
# curl, and Go (to generate the sample PDF).
set -eu

BASE="${1:-http://localhost:8080}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "== sample invoice =="
(cd "$(dirname "$0")/.." && go run ./cmd/mkpdf) > /dev/null 2>&1 || true
SAMPLE="$(dirname "$0")/../invoice.pdf"
cp "$SAMPLE" "$WORK/invoice.pdf"

echo "== GET /healthz =="
curl -s "$BASE/healthz"; echo

echo "== POST /v1/find (query: Total due) =="
curl -s -F file=@"$WORK/invoice.pdf" -F query='Total due' "$BASE/v1/find"; echo

echo "== POST /v1/replace (1,250.00 -> 990.00) =="
curl -s -F file=@"$WORK/invoice.pdf" \
  -F find='Total due: USD 1,250.00' -F with='Total due: USD 990.00' \
  -o "$WORK/replaced.pdf" -w 'HTTP %{http_code}, %{size_download} bytes\n' \
  "$BASE/v1/replace"

echo "== POST /v1/flatten =="
curl -s -F file=@"$WORK/replaced.pdf" \
  -o "$WORK/flattened.pdf" -w 'HTTP %{http_code}, %{size_download} bytes\n' \
  "$BASE/v1/flatten"

echo "Demo outputs left in $WORK until this script exits (replaced.pdf, flattened.pdf)."
