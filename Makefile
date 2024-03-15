# Copyright 2017 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

CMDS=blockplugin
DEPLOY_FOLDER = ./deploy
CMDS=blockplugin
PKG = github.com/kubernetes-csi/csi-driver-block
GINKGO_FLAGS = -ginkgo.v
GO111MODULE = on
GOPATH ?= $(shell go env GOPATH)
GOBIN ?= $(GOPATH)/bin
DOCKER_CLI_EXPERIMENTAL = enabled
export GOPATH GOBIN GO111MODULE DOCKER_CLI_EXPERIMENTAL

include release-tools/build.make

GIT_COMMIT = $(shell git rev-parse HEAD)
BUILD_DATE = $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
IMAGE_VERSION ?= v4.7.0
LDFLAGS = -X ${PKG}/pkg/block.driverVersion=${IMAGE_VERSION} -X ${PKG}/pkg/block.gitCommit=${GIT_COMMIT} -X ${PKG}/pkg/block.buildDate=${BUILD_DATE}
EXT_LDFLAGS = -s -w -extldflags "-static"
# Use a custom version for E2E tests if we are testing in CI
ifdef CI
ifndef PUBLISH
override IMAGE_VERSION := e2e-$(GIT_COMMIT)
endif
endif
IMAGENAME ?= blockplugin
REGISTRY ?= andyzhangx
REGISTRY_NAME ?= $(shell echo $(REGISTRY) | sed "s/.azurecr.io//g")
IMAGE_TAG = $(REGISTRY)/$(IMAGENAME):$(IMAGE_VERSION)
IMAGE_TAG_LATEST = $(REGISTRY)/$(IMAGENAME):latest

E2E_HELM_OPTIONS ?= --set image.block.repository=$(REGISTRY)/$(IMAGENAME) --set image.block.tag=$(IMAGE_VERSION) --set image.block.pullPolicy=Always --set feature.enableInlineVolume=true --set externalSnapshotter.enabled=true
E2E_HELM_OPTIONS += ${EXTRA_HELM_OPTIONS}

# Output type of docker buildx build
OUTPUT_TYPE ?= docker

ALL_ARCH.linux = arm64 amd64 ppc64le
ALL_OS_ARCH = linux-arm64 linux-arm-v7 linux-amd64 linux-ppc64le

.EXPORT_ALL_VARIABLES:

all: block

.PHONY: verify
verify: unit-test
	hack/verify-all.sh

.PHONY: unit-test
unit-test:
	go test -covermode=count -coverprofile=profile.cov ./pkg/... -v

.PHONY: sanity-test
sanity-test: block
	./test/sanity/run-test.sh

.PHONY: local-build-push
local-build-push: block
	docker build -t $(LOCAL_USER)/blockplugin:latest .
	docker push $(LOCAL_USER)/blockplugin

.PHONY: block
block:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -a -ldflags "${LDFLAGS} ${EXT_LDFLAGS}" -mod vendor -o bin/${ARCH}/blockplugin ./cmd/blockplugin

.PHONY: block-armv7
block-armv7:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -a -ldflags "${LDFLAGS} ${EXT_LDFLAGS}" -mod vendor -o bin/arm/v7/blockplugin ./cmd/blockplugin

.PHONY: container-build
container-build:
	docker buildx build --pull --output=type=$(OUTPUT_TYPE) --platform="linux/$(ARCH)" \
		--provenance=false --sbom=false \
		-t $(IMAGE_TAG)-linux-$(ARCH) --build-arg ARCH=$(ARCH) .

.PHONY: container-linux-armv7
container-linux-armv7:
	docker buildx build --pull --output=type=$(OUTPUT_TYPE) --platform="linux/arm/v7" \
		--provenance=false --sbom=false \
		-t $(IMAGE_TAG)-linux-arm-v7 --build-arg ARCH=arm/v7 .

.PHONY: container
container:
	docker buildx rm container-builder || true
	docker buildx create --use --name=container-builder
	# enable qemu for arm64 build
	# https://github.com/docker/buildx/issues/464#issuecomment-741507760
	docker run --privileged --rm tonistiigi/binfmt --uninstall qemu-aarch64
	docker run --rm --privileged tonistiigi/binfmt --install all
	for arch in $(ALL_ARCH.linux); do \
		ARCH=$${arch} $(MAKE) block; \
		ARCH=$${arch} $(MAKE) container-build; \
	done
	$(MAKE) block-armv7
	$(MAKE) container-linux-armv7

.PHONY: push
push:
ifdef CI
	docker manifest create --amend $(IMAGE_TAG) $(foreach osarch, $(ALL_OS_ARCH), $(IMAGE_TAG)-${osarch})
	docker manifest push --purge $(IMAGE_TAG)
	docker manifest inspect $(IMAGE_TAG)
else
	docker push $(IMAGE_TAG)
endif

.PHONY: push-latest
push-latest:
ifdef CI
	docker manifest create --amend $(IMAGE_TAG_LATEST) $(foreach osarch, $(ALL_OS_ARCH), $(IMAGE_TAG)-${osarch})
	docker manifest push --purge $(IMAGE_TAG_LATEST)
	docker manifest inspect $(IMAGE_TAG_LATEST)
else
	docker tag $(IMAGE_TAG) $(IMAGE_TAG_LATEST)
	docker push $(IMAGE_TAG_LATEST)
endif

.PHONY: install-block-server
install-block-server:
	kubectl apply -f ./deploy/example/block-provisioner/block-server.yaml
	kubectl delete secret mount-options --ignore-not-found
	kubectl create secret generic mount-options --from-literal mountOptions="blockvers=4.1"

.PHONY: install-helm
install-helm:
	curl https://raw.githubusercontent.com/helm/helm/master/scripts/get-helm-3 | bash

.PHONY: e2e-bootstrap
e2e-bootstrap: install-helm
	OUTPUT_TYPE=registry $(MAKE) container push
	helm install csi-driver-block ./charts/latest/csi-driver-block --namespace kube-system --wait --timeout=15m -v=5 --debug \
		${E2E_HELM_OPTIONS} \
		--set controller.logLevel=8 \
		--set node.logLevel=8

.PHONY: e2e-teardown
e2e-teardown:
	helm delete csi-driver-block --namespace kube-system

.PHONY: e2e-test
e2e-test:
	if [ ! -z "$(EXTERNAL_E2E_TEST)" ]; then \
		bash ./test/external-e2e/run.sh;\
	else \
		go test -v -timeout=0 ./test/e2e ${GINKGO_FLAGS};\
	fi
