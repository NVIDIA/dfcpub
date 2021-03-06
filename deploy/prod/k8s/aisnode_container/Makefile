#
# Usage:
#  $ env IMAGE_TAG=3.3 make -e all
#  $ env IMAGE_TAG=3.3 make -e all_debug
#  $ env IMAGE_REPO=docker.io/aistore/admin IMAGE_TAG=4.0 make -e all
#

IMAGE_TAG=.must_set_in_environment
IMAGE_REPO=aistore/aisnode

DOCKER=docker

build:
	$(DOCKER) build --quiet \
		-f Dockerfile \
		-t $(IMAGE_REPO):$(IMAGE_TAG) \
		--build-arg providers=$(AIS_BACKEND_PROVIDERS) \
		"../../../../."

build_debug:
	$(DOCKER) build --quiet \
		-f Dockerfile \
		-t $(IMAGE_REPO):$(IMAGE_TAG)-debug \
		--build-arg mode="debug" --build-arg providers=$(AIS_BACKEND_PROVIDERS) \
		"../../../../."

push:
	docker push $(IMAGE_REPO):$(IMAGE_TAG)

push_debug:
	docker push $(IMAGE_REPO):$(IMAGE_TAG)-debug

all: build push

all_debug: build_debug push_debug
