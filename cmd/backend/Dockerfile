FROM golang:1.21-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/backend ./cmd/backend
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /backend ./cmd/backend

FROM alpine:3.20

RUN apk add --no-cache ca-certificates

COPY --from=builder /backend /backend

EXPOSE 8080
ENTRYPOINT ["/backend"]
