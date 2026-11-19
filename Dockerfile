# Build stage
FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/pdfdesk-api .

# Runtime stage: poppler-utils provides pdftotext (text extraction) and
# pdftoppm (flatten rendering).
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends poppler-utils ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/pdfdesk-api /usr/local/bin/pdfdesk-api
EXPOSE 8080
ENTRYPOINT ["pdfdesk-api"]
