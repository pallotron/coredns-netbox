FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git make

# Clone CoreDNS
ARG COREDNS_VERSION=v1.12.0
RUN git clone --depth 1 --branch ${COREDNS_VERSION} https://github.com/coredns/coredns.git /coredns

WORKDIR /coredns

# Replace plugin.cfg with our custom one
COPY plugin.cfg /coredns/plugin.cfg

# Generate plugin imports and build
RUN go generate && go mod tidy && make

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /coredns/coredns /coredns

EXPOSE 53 53/udp
ENTRYPOINT ["/coredns"]
