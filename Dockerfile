# syntax=docker/dockerfile:1

# Stage 1: Build assets and binary
FROM golang:1.25-bookworm AS builder

WORKDIR /app

# Install build dependencies
# 1. templ
RUN go install github.com/a-h/templ/cmd/templ@latest

# 2. tailwindcss (standalone)
RUN curl -sLO https://github.com/tailwindlabs/tailwindcss/releases/latest/download/tailwindcss-linux-x64 && \
    chmod +x tailwindcss-linux-x64 && \
    mv tailwindcss-linux-x64 /usr/local/bin/tailwindcss

# Copy Go module files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Generate Templ templates
RUN templ generate

# Build Tailwind CSS
# Ensure the output directory exists (it should from COPY, but to be safe)
RUN mkdir -p web/static/css && \
    tailwindcss -i web/static/css/input.css -o web/static/css/style.css --minify

# Build Go binary
# CGO_ENABLED=0 for static binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/metra .

# Stage 2: Runtime image
FROM gcr.io/distroless/static-debian12

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/metra .

# Copy static assets (JS, CSS, Images)
# The application expects web/static to be relative to the binary/workdir
COPY --from=builder /app/web/static ./web/static

# Expose port
EXPOSE 8080

# Run
CMD ["./metra"]