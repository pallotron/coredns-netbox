FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git make

# Clone CoreDNS and pre-download its own dependencies.
# Cached as long as COREDNS_VERSION doesn't change.
ARG COREDNS_VERSION=v1.12.0
RUN git clone --depth 1 --branch ${COREDNS_VERSION} https://github.com/coredns/coredns.git /coredns

WORKDIR /coredns
RUN go mod download

# Inject our plugin list and generate zplugins.go.
# Cached as long as plugin.cfg doesn't change.
COPY coredns/plugin.cfg /coredns/plugin.cfg
RUN go mod edit -require github.com/pallotron/coredns-netbox@v0.0.0 \
    && go mod edit -replace github.com/pallotron/coredns-netbox=/coredns-netbox \
    && go generate

# Copy local module last — only this layer and the final build invalidate on source changes.
COPY . /coredns-netbox
RUN go mod tidy && make

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /coredns/coredns /coredns

EXPOSE 53 53/udp
ENTRYPOINT ["/coredns"]
