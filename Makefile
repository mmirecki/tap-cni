deps-update:
	go mod tidy && \
	go mod vendor

gofmt:
	@echo "Running gofmt"
	gofmt -w -s -l `find . -path ./vendor -prune -o -type f -name '*.go' -print`
	goimports -w tap/

build-bin:
	./build.sh

test: build-bin
	sudo -E bash -c "umask 0; PATH=${GOPATH}/bin:$(pwd)/bin:${PATH} go test ./tap/"
