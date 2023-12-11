COMMIT_SHA=$(shell git rev-parse HEAD)

docker:
	buildctl build \
		--frontend dockerfile.v0 \
		--local dockerfile=. \
		--local context=. \
		--output type=image,name=registry.moonrhythm.io/geth-gateway:$(COMMIT_SHA),push=true
