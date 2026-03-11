FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o stateless-executor .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        libsecp256k1-1 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /src/stateless-executor /usr/local/bin/stateless-executor
EXPOSE 8080
ENTRYPOINT ["stateless-executor"]
