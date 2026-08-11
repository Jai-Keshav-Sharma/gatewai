# Gatewai — multi-stage build.
# Stage 1: build the static binary.
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /gatewai ./cmd/gatewai

# Stage 2: minimal runtime image.
FROM gcr.io/distroless/static-debian12
# CRITICAL: copy CA certificates from the builder stage. Distroless/scratch
# images carry NO root certificates — without this line, every upstream TLS
# call to OpenAI/Anthropic/Gemini fails with x509: certificate signed by
# unknown authority.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /gatewai /gatewai
EXPOSE 8080
ENTRYPOINT ["/gatewai"]
