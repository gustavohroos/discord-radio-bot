FROM golang:1.23.3-bullseye AS builder

RUN apt-get update && apt-get install -y \
    build-essential \
    pkg-config \
    libopus-dev \
    ffmpeg

WORKDIR /app

COPY . .

RUN go mod tidy

RUN go build -o main .

FROM debian:bullseye-slim

RUN apt-get update && apt-get install -y \
    libopus0 \
    ffmpeg \
    ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /app/main .
COPY .env /app/.env
COPY radios.json /app/radios.json

EXPOSE 8080

CMD ["./main"]
