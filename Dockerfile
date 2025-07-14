# Use official Go image as base
FROM golang:1.24 AS builder

# Set working directory
WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o webhook-server main.go

# Use a minimal base image for the final stage
FROM alpine:3.18

# Install ca-certificates for HTTPS
RUN apk --no-cache add ca-certificates

# Set working directory
WORKDIR /app

# Copy the binary from builder stage
COPY --from=builder /app/webhook-server .

# Copy TLS certificates (assuming they are in a certs directory)
COPY certs/ /etc/webhook/certs/

# Command to run the webhook server
CMD ["./webhook-server", "--port=8443", "--tls-cert=/etc/webhook/certs/tls.crt", "--tls-key=/etc/webhook/certs/tls.key"]