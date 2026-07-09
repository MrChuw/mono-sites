FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETARCH

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -ldflags="-s -w" -o uploadserver .

ARG TARGETPLATFORM
FROM --platform=$TARGETPLATFORM alpine:3.24.1

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
