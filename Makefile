.PHONY: build build.sidecar build.analyzer test.unit test.e2e test.e2e.local test.helm proto \
       dev dev.cluster dev.netbox dev.token dev.seed dev.images dev.pod-services dev.deploy dev.wait dev.teardown \
       dev.shell dev.shell.sidecar \
       lint clean

# Go settings
GOBIN := $(shell go env GOPATH)/bin
MODULE := github.com/pallotron/coredns-netbox

# Docker image tags
COREDNS_IMAGE := coredns-netbox:dev
SIDECAR_IMAGE := coredns-netbox-sidecar:dev

# Helm
HELM_RELEASE := coredns-netbox
HELM_NAMESPACE := coredns-netbox
K3D_CLUSTER := coredns-netbox
NETBOX_NAMESPACE := netbox

# Prevent accidental deploys to company clusters. All dev kubectl/helm commands
# use this context explicitly — they will fail if the cluster does not exist.
DEV_CONTEXT := k3d-$(K3D_CLUSTER)
KUBECTL := kubectl --context $(DEV_CONTEXT)
HELM    := helm --kube-context $(DEV_CONTEXT)

# Netbox chart and app version — must match the fixture image tag
NETBOX_CHART_VERSION := 8.2.15
NETBOX_APP_VERSION   := 4.6.0

# Shared secret name for the Netbox API token
NETBOX_TOKEN_SECRET := netbox-api-token

# ---------- Build ----------

build: build.sidecar build.analyzer

build.sidecar:
	go build -o bin/sidecar ./cmd/sidecar/

build.analyzer:
	go build -o bin/analyzer ./cmd/analyzer/

# ---------- Test ----------

test.unit:
	go test ./internal/... -v -count=1 -race

# test.e2e: raw test runner — used by CI (which sets up the environment separately)
test.e2e:
	STRIP_DC_LABEL=true DC_LABEL_REWRITE=true GRPC_AUTH_TOKEN=devtoken \
	NAME_TEMPLATES=true \
	COREDNS_RELOAD_ADDR=127.0.0.1:18054 \
	DNS_POD0=127.0.0.1:15360 \
	DNS_POD1=127.0.0.1:15361 \
	go test ./tests/e2e/... -v -count=1 -tags=e2e

# test.e2e.local: full local loop — rebuilds images, redeploys, waits, then runs e2e
# Requires: make dev.cluster dev.netbox dev.token dev.seed (one-time setup)
test.e2e.local: dev.images dev.deploy dev.wait test.e2e

proto:
	protoc --go_out=. --go_opt=paths=source_relative \
	       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	       proto/coredns_netbox/v1/zones.proto

test.helm:
	helm lint ./helm/coredns-netbox --set netbox.token=test
	helm template test ./helm/coredns-netbox --set netbox.token=test > /dev/null
	helm template test ./helm/coredns-netbox --set netbox.existingSecret=my-secret > /dev/null
	@helm template test ./helm/coredns-netbox 2>&1 | grep -q "netbox.token or netbox.existingSecret must be set" && \
		echo "PASS: credential guard fires correctly" || \
		(echo "FAIL: credential guard did not fire" && exit 1)
	helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set secondary.enabled=true \
		--set 'secondary.zones[0]=example.org' \
		--set 'secondary.transferFrom[0]=10.0.0.1' > /dev/null
	helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set metrics.enabled=false > /dev/null
	# service.annotations renders on ClusterIP service
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set 'service.annotations.example\.com/foo=bar' 2>&1 | grep -q "example.com/foo" && \
		echo "PASS: service.annotations rendered on ClusterIP service" || \
		(echo "FAIL: service.annotations not rendered" && exit 1)
	# service.external.enabled renders the external LoadBalancer service
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set service.external.enabled=true 2>&1 | grep -q "coredns-netbox-external" && \
		echo "PASS: external service rendered" || \
		(echo "FAIL: external service not rendered" && exit 1)
	# service.external.annotations renders on external service only
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set service.external.enabled=true \
		--set 'service.external.annotations.example\.com/foo=bar' 2>&1 | grep -q "example.com/foo" && \
		echo "PASS: service.external.annotations rendered" || \
		(echo "FAIL: service.external.annotations not rendered" && exit 1)
	# metrics.enabled adds metrics ports
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set metrics.enabled=true 2>&1 | grep -q "name: metrics" && \
		echo "PASS: metrics ports rendered" || \
		(echo "FAIL: metrics ports not rendered" && exit 1)
	# transfer.to renders the transfer block; no notify directive is ever
	# emitted (the CoreDNS transfer plugin rejects it at startup)
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set 'transfer.to[0]=10.0.1.5' 2>&1 | grep -q "to 10.0.1.5" && \
		echo "PASS: transfer.to rendered" || \
		(echo "FAIL: transfer.to not rendered" && exit 1)
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set 'transfer.to[0]=10.0.1.5' \
		--set 'transfer.notify[0]=10.0.1.6' 2>&1 | grep -q "notify" && \
		(echo "FAIL: notify directive rendered in transfer block" && exit 1) || \
		echo "PASS: no notify directive in transfer block"
	# soa values render as sidecar env vars
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set soa.refresh=300 2>&1 | grep -A1 "SOA_REFRESH" | grep -q '"300"' && \
		echo "PASS: soa.refresh rendered as SOA_REFRESH env" || \
		(echo "FAIL: SOA_REFRESH env not rendered" && exit 1)
	# transfer block absent when to is not set
	@helm template test ./helm/coredns-netbox --set netbox.token=test 2>&1 | grep -qv "transfer {" && \
		echo "PASS: transfer block absent when empty" || \
		(echo "FAIL: transfer block present when it should be absent" && exit 1)
	# coredns.extraConfig injects directive into primary server block
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set 'coredns.extraConfig=rewrite name exact foo.example.com. bar.example.com.' 2>&1 | grep -q "rewrite name exact" && \
		echo "PASS: coredns.extraConfig rendered" || \
		(echo "FAIL: coredns.extraConfig not rendered" && exit 1)
	# coredns.extraConfig absent when empty
	@helm template test ./helm/coredns-netbox --set netbox.token=test 2>&1 | grep -qv "extraConfig" && \
		echo "PASS: coredns.extraConfig absent when empty" || \
		(echo "FAIL: coredns.extraConfig present when it should be absent" && exit 1)
	# netboxreload directive present in Corefile
	@helm template test ./helm/coredns-netbox --set netbox.token=test 2>&1 | grep -q "netboxreload {" && \
		echo "PASS: netboxreload in Corefile" || \
		(echo "FAIL: netboxreload missing from Corefile" && exit 1)
	# grpc-reload port declared on coredns container
	@helm template test ./helm/coredns-netbox --set netbox.token=test 2>&1 | grep -q "name: grpc-reload" && \
		echo "PASS: grpc-reload port declared" || \
		(echo "FAIL: grpc-reload port missing" && exit 1)
	# standalone mode generates per-pod reload addresses
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set sidecar.standalone=true --set replicaCount=2 2>&1 | grep -A1 "COREDNS_RELOAD_ADDRS" | grep -q "svc.cluster.local" && \
		echo "PASS: standalone mode generates per-pod reload addrs" || \
		(echo "FAIL: standalone mode reload addrs wrong" && exit 1)
	# standalone mode: source_url in Corefile, fetch-from in zone-init, PVC created
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set sidecar.standalone=true 2>&1 | grep -q "source_url" && \
		echo "PASS: source_url in Corefile when standalone" || \
		(echo "FAIL: source_url missing from Corefile" && exit 1)
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set sidecar.standalone=true 2>&1 | grep -q "fetch-from" && \
		echo "PASS: --fetch-from in zone-init when standalone" || \
		(echo "FAIL: --fetch-from missing from zone-init" && exit 1)
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set sidecar.standalone=true 2>&1 | grep "zone-init" -A 20 | grep -qv "NETBOX_TOKEN" && \
		echo "PASS: NETBOX_TOKEN absent from standalone zone-init env" || \
		(echo "FAIL: NETBOX_TOKEN present in standalone zone-init env" && exit 1)
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set sidecar.standalone=true 2>&1 | grep -q "sidecar-data" && \
		echo "PASS: sidecar PVC rendered when standalone" || \
		(echo "FAIL: sidecar PVC missing" && exit 1)
	# non-standalone mode: directory in Corefile, run-once in zone-init, no sidecar PVC
	@helm template test ./helm/coredns-netbox --set netbox.token=test 2>&1 | grep -q "directory /zones" && \
		echo "PASS: directory in Corefile when non-standalone" || \
		(echo "FAIL: directory missing from Corefile" && exit 1)
	# NetworkPolicy renders when networkPolicy.enabled and standalone
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set networkPolicy.enabled=true --set sidecar.standalone=true 2>&1 | grep -q "kind: NetworkPolicy" && \
		echo "PASS: NetworkPolicy rendered when enabled+standalone" || \
		(echo "FAIL: NetworkPolicy not rendered" && exit 1)
	# deviceNameParsers render as sidecar env vars (--set-json: the plain --set
	# parser chokes on values starting with '{')
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set 'deviceNameParsers[0]=^(?P<dc>[a-z0-9]+)-r(?P<rack>[0-9]+)$$' \
		--set-json 'nameFormats.canonical="{{.name}}.{{.domain}}"' 2>&1 | grep -q "DEVICE_NAME_PARSERS" && \
		echo "PASS: deviceNameParsers render as DEVICE_NAME_PARSERS env" || \
		(echo "FAIL: DEVICE_NAME_PARSERS not rendered" && exit 1)
	@helm template test ./helm/coredns-netbox --set netbox.token=test 2>&1 | ( ! grep -q "DEVICE_NAME_PARSERS" ) && \
		echo "PASS: DEVICE_NAME_PARSERS absent by default" || \
		(echo "FAIL: DEVICE_NAME_PARSERS rendered without config" && exit 1)
	@echo "Helm chart tests passed."

# ---------- Lint ----------

lint:
	golangci-lint run ./...

# ---------- Docker ----------

dev.images: dev.images.coredns dev.images.sidecar

dev.images.coredns:
	docker build -t $(COREDNS_IMAGE) -f coredns/Dockerfile .
	k3d image import $(COREDNS_IMAGE) -c $(K3D_CLUSTER)

dev.images.sidecar:
	docker build -t $(SIDECAR_IMAGE) -f docker/sidecar/Dockerfile .
	k3d image import $(SIDECAR_IMAGE) -c $(K3D_CLUSTER)

# ---------- Dev Environment ----------

dev.cluster:
	k3d cluster create --config dev/k3d-config.yaml || true
	@echo "Cluster $(K3D_CLUSTER) is ready"


dev.netbox:
	helm repo add netbox-community https://netbox-community.github.io/netbox-chart/ || true
	helm repo update
	$(KUBECTL) create namespace $(NETBOX_NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) apply -f dev/netbox-extra-configmap.yaml
	$(HELM) upgrade --install netbox netbox-community/netbox \
		-n $(NETBOX_NAMESPACE) \
		--version $(NETBOX_CHART_VERSION) \
		-f dev/netbox-values.yaml \
		--wait --timeout 10m
	@echo "Netbox deployed."

# Create a v2 API token in Netbox and store it in a shared Kubernetes Secret.
# Netbox 4.x v2 tokens are HMAC-hashed; the plaintext is only available at
# creation time, so we capture it and store it for the sidecar.
dev.token:
	@echo "Creating Netbox API token..."
	@SCRIPT=$$(cat dev/create-token.py) && \
	NETBOX_TOKEN=$$($(KUBECTL) exec -n $(NETBOX_NAMESPACE) deploy/netbox -c netbox -- \
		python /opt/netbox/netbox/manage.py shell --no-startup --no-imports \
		-c "$$SCRIPT" \
		2>/dev/null | grep '^nbt_') && \
	echo "Token: $$NETBOX_TOKEN" && \
	$(KUBECTL) create namespace $(HELM_NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f - && \
	$(KUBECTL) create secret generic $(NETBOX_TOKEN_SECRET) \
		-n $(NETBOX_NAMESPACE) \
		--from-literal=token="$$NETBOX_TOKEN" \
		--dry-run=client -o yaml | $(KUBECTL) apply -f - && \
	$(KUBECTL) create secret generic $(NETBOX_TOKEN_SECRET) \
		-n $(HELM_NAMESPACE) \
		--from-literal=token="$$NETBOX_TOKEN" \
		--dry-run=client -o yaml | $(KUBECTL) apply -f - && \
	echo "Token stored in secret '$(NETBOX_TOKEN_SECRET)' in namespaces: $(NETBOX_NAMESPACE), $(HELM_NAMESPACE)"

dev.seed:
	@echo "Seeding Netbox with 18,000 IP addresses via Django ORM..."
	@SCRIPT=$$(cat dev/seed-ips.py) && \
	$(KUBECTL) exec -n $(NETBOX_NAMESPACE) deploy/netbox -c netbox -- \
		python /opt/netbox/netbox/manage.py shell --no-startup --no-imports \
		-c "$$SCRIPT"

dev.pod-services:
	$(KUBECTL) create namespace $(HELM_NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) apply -f dev/coredns-per-pod-services.yaml

dev.deploy: dev.pod-services
	$(KUBECTL) create namespace $(HELM_NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(HELM) upgrade --install $(HELM_RELEASE) ./helm/coredns-netbox \
		-n $(HELM_NAMESPACE) \
		-f dev/coredns-netbox-values.yaml \
		--set netbox.existingSecret=$(NETBOX_TOKEN_SECRET) \
		--wait --timeout 5m

dev.wait:
	@echo "Waiting for primary DNS (port 15353) to serve correct answers..."
	@for i in $$(seq 1 30); do \
		if dig @127.0.0.1 -p 15353 +tcp +time=2 server1-mgmt.mycompany.com A 2>/dev/null | grep -q "10.1.0.1"; then \
			echo "Primary DNS ready."; break; \
		fi; \
		echo "  attempt $$i/30 — retrying in 5s..."; sleep 5; \
	done
	@echo "Waiting for secondary DNS (port 15354) to complete zone transfer..."
	@for i in $$(seq 1 30); do \
		if dig @127.0.0.1 -p 15354 +tcp +time=2 server1-mgmt.mycompany.com A 2>/dev/null | grep -q "10.1.0.1"; then \
			echo "Secondary DNS ready."; break; \
		fi; \
		echo "  attempt $$i/30 — retrying in 5s..."; sleep 5; \
	done

dev: dev.cluster dev.netbox dev.token dev.seed dev.images dev.deploy
	@echo "Full dev environment is ready!"
	@echo "DNS:  dig @127.0.0.1 -p 15353 server1-mgmt.mycompany.com A"
	@echo "gRPC: grpcurl -plaintext -H 'authorization: bearer devtoken' 127.0.0.1:18083 coredns_netbox.v1.ControlService/GetStatus"

dev.shell:
	$(KUBECTL) debug -it -n $(HELM_NAMESPACE) coredns-netbox-0 \
		--image=busybox --target=coredns --profile=general -- sh

dev.shell.sidecar:
	$(KUBECTL) debug -it -n $(HELM_NAMESPACE) coredns-netbox-0 \
		--image=busybox --target=sidecar --profile=general -- sh

dev.teardown:
	k3d cluster delete $(K3D_CLUSTER)

# ---------- Clean ----------

clean:
	rm -rf bin/
