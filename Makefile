.PHONY: build build.sidecar build.analyzer test.unit test.e2e test.helm proto \
       dev dev.cluster dev.netbox dev.token dev.seed dev.images dev.deploy dev.wait dev.teardown \
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

test.e2e:
	STRIP_DC_LABEL=true DC_LABEL_REWRITE=true GRPC_AUTH_TOKEN=devtoken go test ./tests/e2e/... -v -count=1 -tags=e2e

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
	# transfer.notify renders notify line in transfer block
	@helm template test ./helm/coredns-netbox --set netbox.token=test \
		--set 'transfer.to[0]=*' \
		--set 'transfer.notify[0]=10.0.1.5' \
		--set 'transfer.notify[1]=10.0.1.6' 2>&1 | grep -q "notify 10.0.1.5 10.0.1.6" && \
		echo "PASS: transfer.notify rendered" || \
		(echo "FAIL: transfer.notify not rendered" && exit 1)
	# transfer block absent when neither to nor notify is set
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
	@echo "Helm chart tests passed."

# ---------- Lint ----------

lint:
	golangci-lint run ./...

# ---------- Docker ----------

dev.images: dev.images.coredns dev.images.sidecar

dev.images.coredns:
	docker build -t $(COREDNS_IMAGE) -f coredns/Dockerfile coredns/
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
	kubectl create namespace $(NETBOX_NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f dev/netbox-extra-configmap.yaml
	helm upgrade --install netbox netbox-community/netbox \
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
	NETBOX_TOKEN=$$(kubectl exec -n $(NETBOX_NAMESPACE) deploy/netbox -c netbox -- \
		python /opt/netbox/netbox/manage.py shell --no-startup --no-imports \
		-c "$$SCRIPT" \
		2>/dev/null | grep '^nbt_') && \
	echo "Token: $$NETBOX_TOKEN" && \
	kubectl create namespace $(HELM_NAMESPACE) --dry-run=client -o yaml | kubectl apply -f - && \
	kubectl create secret generic $(NETBOX_TOKEN_SECRET) \
		-n $(NETBOX_NAMESPACE) \
		--from-literal=token="$$NETBOX_TOKEN" \
		--dry-run=client -o yaml | kubectl apply -f - && \
	kubectl create secret generic $(NETBOX_TOKEN_SECRET) \
		-n $(HELM_NAMESPACE) \
		--from-literal=token="$$NETBOX_TOKEN" \
		--dry-run=client -o yaml | kubectl apply -f - && \
	echo "Token stored in secret '$(NETBOX_TOKEN_SECRET)' in namespaces: $(NETBOX_NAMESPACE), $(HELM_NAMESPACE)"

dev.seed:
	@echo "Seeding Netbox with 18,000 IP addresses via Django ORM..."
	@SCRIPT=$$(cat dev/seed-ips.py) && \
	kubectl exec -n $(NETBOX_NAMESPACE) deploy/netbox -c netbox -- \
		python /opt/netbox/netbox/manage.py shell --no-startup --no-imports \
		-c "$$SCRIPT"

dev.deploy:
	kubectl create namespace $(HELM_NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	helm upgrade --install $(HELM_RELEASE) ./helm/coredns-netbox \
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
	kubectl debug -it -n $(HELM_NAMESPACE) coredns-netbox-0 \
		--image=busybox --target=coredns --profile=general -- sh

dev.shell.sidecar:
	kubectl debug -it -n $(HELM_NAMESPACE) coredns-netbox-0 \
		--image=busybox --target=sidecar --profile=general -- sh

dev.teardown:
	k3d cluster delete $(K3D_CLUSTER)

# ---------- Clean ----------

clean:
	rm -rf bin/
