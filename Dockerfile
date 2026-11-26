FROM golang:1.23.4-alpine3.21 AS builder

WORKDIR /app

COPY . .

# Build the application
RUN go build -ldflags="-s -w" -o fengchuan .

# Use a minimal alpine image for running
FROM alpine:3.21

WORKDIR /app

# FFmpeg is used to generate cached video poster images.
RUN apk add --no-cache ffmpeg ca-certificates \
    && addgroup -S fengchuan \
    && adduser -S -G fengchuan fengchuan \
    && mkdir -p /app/data \
    && chown -R fengchuan:fengchuan /app

# Copy the binary from builder
COPY --from=builder /app/fengchuan .
USER fengchuan

# Expose the default port
EXPOSE 8080

# Run the server
ENTRYPOINT ["/app/fengchuan"]
