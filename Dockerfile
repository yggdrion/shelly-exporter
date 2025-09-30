# Multi-stage build
FROM golang:1.24-alpine AS builder

# Install dependencies
RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY *.go ./

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o shelly-exporter main.go

# Final stage
FROM alpine:latest

# Install runtime dependencies (ca-certificates for HTTPS API calls)
RUN apk add --no-cache ca-certificates

WORKDIR /app

# Copy the binary from builder stage
COPY --from=builder /app/shelly-exporter .

# Expose port
EXPOSE 8080

# Run the binary
CMD ["./shelly-exporter"]
