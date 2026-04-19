.PHONY: build build.sidecar build.analyzer test.unit test.e2e test.helm proto \
       dev dev.cluster dev.netbox dev.token dev.seed dev.images dev.deploy dev.wait dev.teardown \
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
	go test ./tests/e2e/... -v -count=1 -tags=e2e

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
		if dig @127.0.0.1 -p 15353 +tcp +time=2 server1-mgmt.dc1.mycompany.com A 2>/dev/null | grep -q "10.1.0.1"; then \
			echo "Primary DNS ready."; break; \
		fi; \
		echo "  attempt $$i/30 — retrying in 5s..."; sleep 5; \
	done
	@echo "Waiting for secondary DNS (port 15354) to complete zone transfer..."
	@for i in $$(seq 1 30); do \
		if dig @127.0.0.1 -p 15354 +tcp +time=2 server1-mgmt.dc1.mycompany.com A 2>/dev/null | grep -q "10.1.0.1"; then \
			echo "Secondary DNS ready."; break; \
		fi; \
		echo "  attempt $$i/30 — retrying in 5s..."; sleep 5; \
	done

dev: dev.cluster dev.netbox dev.token dev.seed dev.images dev.deploy
	@echo "Full dev environment is ready!"
	@echo "Test with: dig @127.0.0.1 -p 15353 server1-mgmt.dc1.mycompany.com A"

dev.teardown:
	k3d cluster delete $(K3D_CLUSTER)

# ---------- Clean ----------

clean:
	rm -rf bin/
