# ---- Build stage ----
FROM golang:1.23-alpine AS builder
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/mcp-gateway ./cmd/main.go

# ---- Final stage ----
FROM alpine:3.20
RUN apk --no-cache add ca-certificates curl
RUN addgroup -g 1001 -S appgroup && adduser -u 1001 -S appuser -G appgroup
WORKDIR /app
COPY --from=builder /out/mcp-gateway .
COPY config.yaml .
USER appuser
EXPOSE 8081 8086
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8086/healthz || exit 1
ENTRYPOINT ["./mcp-gateway"]