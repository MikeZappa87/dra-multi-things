# DRA CDI netDevices Demo Makefile

DRIVER_NAME := dra-driver
IMAGE_TAG := latest
KIND_CLUSTER := dra-demo
NAMESPACE := dra-system
KIND_NODE_IMAGE := kindest/node:v1.35.0-runc1.4
export PATH := $(PATH):$(shell go env GOPATH)/bin

.PHONY: all build load deploy undeploy clean cluster cluster-delete logs help kind-image network-test network-check

all: cluster build load deploy network-test

help:
	@echo "DRA CDI netDevices Demo - make help for usage"

kind-image:
	@echo "Building custom kind node image..."
	docker build -t $(KIND_NODE_IMAGE) ./kind-node/

cluster:
	@if ! docker image inspect $(KIND_NODE_IMAGE) >/dev/null 2>&1; then $(MAKE) kind-image; fi
	@command -v kind >/dev/null 2>&1 || go install sigs.k8s.io/kind@latest
	kind create cluster --config kind-config.yaml
	kubectl wait --for=condition=Ready nodes --all --timeout=120s
	@docker exec $(KIND_CLUSTER)-control-plane runc --version | head -3
	@docker exec $(KIND_CLUSTER)-control-plane containerd --version

cluster-delete:
	kind delete cluster --name $(KIND_CLUSTER)

build:
	docker build -t $(DRIVER_NAME):$(IMAGE_TAG) .

load:
	kind load docker-image $(DRIVER_NAME):$(IMAGE_TAG) --name $(KIND_CLUSTER)

deploy:
	kubectl apply -f deploy/namespace.yaml
	kubectl apply -f deploy/driver.yaml
	kubectl apply -f deploy/resourceclass.yaml
	kubectl wait --for=condition=Ready pod -l app=$(DRIVER_NAME) -n $(NAMESPACE) --timeout=60s || true

undeploy:
	-kubectl delete -f deploy/deployment.yaml --ignore-not-found
	-kubectl delete resourceclaims --all --ignore-not-found
	-kubectl delete -f deploy/resourceclass.yaml --ignore-not-found
	-kubectl delete -f deploy/driver.yaml --ignore-not-found
	-kubectl delete -f deploy/namespace.yaml --ignore-not-found

logs:
	kubectl logs -n $(NAMESPACE) -l app=$(DRIVER_NAME) -f

clean: cluster-delete
	@echo "Cleanup complete"

restart:
	kubectl rollout restart daemonset/$(DRIVER_NAME) -n $(NAMESPACE)
	kubectl wait --for=condition=Ready pod -l app=$(DRIVER_NAME) -n $(NAMESPACE) --timeout=60s

debug:
	@echo "=== Nodes ===" && kubectl get nodes
	@echo "=== DRA Driver ===" && kubectl get pods -n $(NAMESPACE) -o wide
	@echo "=== DeviceClasses ===" && kubectl get deviceclasses
	@echo "=== ResourceSlices ===" && kubectl get resourceslices
	@echo "=== ResourceClaims ===" && kubectl get resourceclaims --all-namespaces
	@echo "=== Test Pods ===" && kubectl get pods -l app=dra-network-test -o wide 2>/dev/null || true

network-test:
	kubectl apply -f deploy/deployment.yaml
	@sleep 5
	kubectl wait --for=condition=Ready pod -l app=dra-network-test --timeout=60s || true

network-check:
	@kubectl get pods -l app=dra-network-test -o wide
	@POD=$$(kubectl get pods -l app=dra-network-test -o jsonpath='{.items[0].metadata.name}' 2>/dev/null); if [ -n "$$POD" ]; then kubectl exec $$POD -- ip link show; echo ""; kubectl exec $$POD -- ip addr show eth1 2>/dev/null || echo "eth1 not found"; fi
	@NODE=$$(kubectl get pods -l app=dra-network-test -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null); if [ -n "$$NODE" ]; then echo "CDI specs on $$NODE:"; docker exec $$NODE ls -la /etc/cdi/ 2>/dev/null || true; fi
