all: build

PREFIX?=registry.aliyuncs.com/acs
FLAGS=
ALL_ARCHITECTURES=amd64 arm arm64 ppc64le s390x
ML_PLATFORMS=linux/amd64,linux/arm,linux/arm64,linux/ppc64le,linux/s390x


GITHUB?=github.com/yonyoucloud/kube-eventer
VERSION?=v1.2.1
GIT_COMMIT:=$(shell git rev-parse --short HEAD)

COMMIT_SHA ?= $(shell git rev-parse --short HEAD)
HOST_ARCH = $(shell which go >/dev/null 2>&1 && go env GOARCH)
HOST_OS = $(shell which go >/dev/null 2>&1 && go env GOOS)
ARCH ?= $(HOST_ARCH)
OS ?= $(HOST_OS)

# 打arm包时强制一下OS，否则Mac下生成的无法在linux arm中运行
OS = linux

REGISTRY ?= ycr.yonyoucloud.com/base
IMAGE ?= $(REGISTRY)/kube-eventer:$(VERSION)-$(COMMIT_SHA)-$(ARCH)

KUBE_EVENTER_LDFLAGS=-w -X github.com/AliyunContainerService/kube-eventer/version.Github=$(GITHUB) -X github.com/AliyunContainerService/kube-eventer/version.Version=$(VERSION) -X github.com/AliyunContainerService/kube-eventer/version.GitCommit=$(GIT_COMMIT)

fmt:
	find . -type f -name "*.go" | grep -v "./vendor*" | xargs gofmt -s -w

build: clean
	# GOARCH=$(ARCH) CGO_ENABLED=0 go build -ldflags "$(KUBE_EVENTER_LDFLAGS)" -o kube-eventer github.com/AliyunContainerService/kube-eventer
	GOARCH=$(ARCH) GOOS=$(OS) CGO_ENABLED=0 go build -ldflags "$(KUBE_EVENTER_LDFLAGS)" -mod=vendor -o ./deploy/$(ARCH)/kube-eventer .

image: clean-image
	echo "Building docker image ($(ARCH))..."
	@docker build \
		--no-cache \
		--build-arg TARGETARCH="$(ARCH)" \
		--build-arg COMMIT_SHA="$(COMMIT_SHA)" \
		-t $(IMAGE) -f deploy/Dockerfile.local deploy

deploy:
	@kubectl apply -f ./deploy/deploy.local.yaml

clean-image:
	echo "removing old image $(REGISTRY)/kube-eventer:*"
	@docker rmi `docker images | grep "$(REGISTRY)/kube-eventer" | awk '{print $$3}'` || true

push:  ## 推送镜像
	@docker push $(IMAGE)

sanitize:
	hack/check_gofmt.sh
	hack/run_vet.sh

test-unit: clean sanitize build

ifeq ($(ARCH),amd64)
	GOARCH=$(ARCH) go test --test.short -race ./... $(FLAGS)
else
	GOARCH=$(ARCH) go test --test.short ./... $(FLAGS)
endif

test-unit-cov: clean sanitize build
	hack/coverage.sh

docker-container:
	docker build --pull -t $(PREFIX)/kube-eventer-$(ARCH):$(VERSION)-$(GIT_COMMIT)-aliyun -f deploy/Dockerfile .

clean:
	rm -f ./deploy/$(ARCH)/kube-eventer

.PHONY: all build sanitize test-unit test-unit-cov docker-container clean fmt
