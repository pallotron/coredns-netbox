FROM golang:1.25-alpine AS builder

ENV GOTOOLCHAIN=auto

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /sidecar ./cmd/sidecar/

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /sidecar /sidecar

ENTRYPOINT ["/sidecar"]
