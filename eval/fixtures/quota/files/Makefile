.PHONY: test vet

# CI runs exactly this.
test:
	go test -race -shuffle=on -count=1 ./...

vet:
	go vet ./...
