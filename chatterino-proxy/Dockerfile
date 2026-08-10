FROM golang:1.26-alpine AS builder

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY go.mod go.sum* ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o proxy .

# --- Stage 2: Imagem Final ZERADA (scratch) ---
FROM scratch

# 1. Copia os Certificados CA (essencial para o proxy fazer requisições HTTPS para os upstreams)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# 2. Copia os dados de Timezone (opcional, mas evita erros de fuso horário em logs)
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# 3. Copia o binário e o arquivo de configuração
COPY --from=builder /app/proxy /proxy
COPY config.json /config.json

# Expõe a porta do Proxy
EXPOSE 8080

# No Scratch não existe /bin/sh, executamos o binário diretamente em formato de array JSON
ENTRYPOINT ["/proxy"]
