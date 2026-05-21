FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git make

# Clone CoreDNS at the pinned version
ARG COREDNS_VERSION=v1.12.0
RUN git clone --depth 1 --branch ${COREDNS_VERSION} https://github.com/coredns/coredns.git /coredns

WORKDIR /coredns

# Replace plugin.cfg with our custom one (includes netboxreload)
COPY coredns/plugin.cfg /coredns/plugin.cfg

# Copy the local coredns-netbox module so CoreDNS can resolve it via replace
COPY . /coredns-netbox

# Add a replace directive pointing CoreDNS at our local module, then tidy and build
RUN go mod edit -require github.com/pallotron/coredns-netbox@v0.0.0 \
    && go mod edit -replace github.com/pallotron/coredns-netbox=/coredns-netbox \
    && go generate \
    && go mod tidy \
    && make

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /coredns/coredns /coredns

EXPOSE 53 53/udp
ENTRYPOINT ["/coredns"]
