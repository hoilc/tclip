# Build stage
FROM golang:1.23-bookworm AS builder

WORKDIR /app

# Copy go.mod and go.sum first to leverage Docker cache
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /tclipd ./cmd/tclipd

# Final stage
FROM gcr.io/distroless/static

# Copy the binary from builder
COPY --from=builder /tclipd /bin/tclipd

# The distroless image doesn't have a shell, so use the binary as entrypoint
CMD ["/bin/tclipd"]
