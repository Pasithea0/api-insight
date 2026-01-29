FROM golang:1.23-alpine AS builder

WORKDIR /app

# Pre-cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source and build a static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o apiinsight .

FROM alpine:3.20

RUN adduser -D -g '' appuser \
  && apk add --no-cache ca-certificates tzdata wget

WORKDIR /app

COPY --from=builder /app/apiinsight /app/apiinsight

USER appuser

ENV APP_LISTEN_ADDR=":8080"

EXPOSE 8080

ENTRYPOINT ["/app/apiinsight"]
