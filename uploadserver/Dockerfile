FROM golang:1.26-alpine AS builder

WORKDIR /app

RUN apk add --no-cache gcc musl-dev sqlite-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=1 GOOS=linux \
    go build \
    -ldflags="-s -w" \
    -o uploadserver .

FROM alpine:3.24.1

WORKDIR /app

RUN apk add --no-cache \
    exiftool \
    vips-tools \
    ffmpeg \
    ca-certificates \
    sqlite-libs \
    tzdata

COPY --from=builder /app/uploadserver .

EXPOSE 8000
CMD ["./uploadserver"]
