.PHONY: build build.sidecar build.seed test.unit test.e2e \
       dev dev.cluster dev.netbox dev.token dev.seed dev.images dev.deploy dev.teardown \
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

build: build.sidecar build.seed

build.sidecar:
	go build -o bin/sidecar ./cmd/sidecar/

build.seed:
	go build -o bin/seed ./cmd/seed/

# ---------- Test ----------

test.unit:
	go test ./internal/... -v -count=1

test.e2e:
	go test ./tests/e2e/... -v -count=1 -tags=e2e

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
# creation time, so we capture it and store it for the seed tool and sidecar.
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
	@echo "Port-forwarding to Netbox..."
	@kubectl port-forward -n $(NETBOX_NAMESPACE) svc/netbox 8080:80 > /dev/null 2>&1 & \
	PF_PID=$$!; \
	sleep 3; \
	NETBOX_TOKEN=$$(kubectl get secret $(NETBOX_TOKEN_SECRET) -n $(NETBOX_NAMESPACE) -o jsonpath='{.data.token}' | base64 -d) && \
	NETBOX_URL=http://localhost:8080 \
	NETBOX_TOKEN=$$NETBOX_TOKEN \
	go run ./cmd/seed/; \
	kill $$PF_PID 2>/dev/null || true

dev.deploy:
	kubectl create namespace $(HELM_NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -
	PRIMARY_IP=$$(kubectl get svc $(HELM_RELEASE) -n $(HELM_NAMESPACE) -o jsonpath='{.spec.clusterIP}' 2>/dev/null); \
	helm upgrade --install $(HELM_RELEASE) ./helm/coredns-netbox \
		-n $(HELM_NAMESPACE) \
		--set netbox.existingSecret=$(NETBOX_TOKEN_SECRET) \
		--set transfer.to[0]='*' \
		--set secondary.enabled=true \
		--set secondary.zones[0]=example.org \
		$${PRIMARY_IP:+--set secondary.transferFrom[0]=$$PRIMARY_IP} \
		--wait --timeout 5m

dev: dev.cluster dev.netbox dev.token dev.seed dev.images dev.deploy
	@echo "Full dev environment is ready!"
	@echo "Test with: dig @127.0.0.1 -p 15353 host1.example.org A"

dev.teardown:
	k3d cluster delete $(K3D_CLUSTER)

# ---------- Clean ----------

clean:
	rm -rf bin/
