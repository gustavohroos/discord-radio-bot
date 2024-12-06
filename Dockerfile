# Build stage
FROM golang:1.23.3 AS builder

# Install necessary packages, including opus and FFmpeg libraries
RUN apt-get update && apt-get install -y \
    build-essential \
    pkg-config \
    libopus-dev \
    ffmpeg

WORKDIR /app

# Copy source code
COPY . .

RUN go mod tidy

RUN go build -o main .

# Final stage
FROM ubuntu:22.04

# Install runtime dependencies, including CA certificates
RUN apt-get update && apt-get install -y \
    libopus0 \
    ffmpeg \
    ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy the built binary
COPY --from=builder /app/main .
COPY .env /app/.env

# Expose application port (adjust as needed)
EXPOSE 8080

# Run the application
CMD ["./main"]