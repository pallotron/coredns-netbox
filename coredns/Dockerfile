FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git make

# Clone CoreDNS
ARG COREDNS_VERSION=v1.12.0
RUN git clone --depth 1 --branch ${COREDNS_VERSION} https://github.com/coredns/coredns.git /coredns

WORKDIR /coredns

# Replace plugin.cfg with our custom one (includes netbox plugin)
COPY plugin.cfg /coredns/plugin.cfg

# Generate plugin imports, fetch the netbox plugin, and build
RUN go generate && go get github.com/oz123/coredns-netbox-plugin && go mod tidy && make

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /coredns/coredns /coredns

EXPOSE 53 53/udp
ENTRYPOINT ["/coredns"]
