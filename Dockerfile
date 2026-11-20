FROM golang:alpine AS builder

WORKDIR /app

COPY . .

# Build the application
RUN go build -ldflags="-s -w" -o fengchuan .

# Use a minimal alpine image for running
FROM alpine:latest

WORKDIR /app

# FFmpeg is used to generate cached video poster images.
RUN apk add --no-cache ffmpeg ca-certificates && mkdir -p /app/data

# Copy the binary from builder
COPY --from=builder /app/fengchuan .

# Expose the default port
EXPOSE 8080

# Run the server
CMD ["/app/fengchuan"]
