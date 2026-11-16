.PHONY: build vet run demo verify-import
build:
	go build ./...
vet:
	go vet ./...
run:
	go run ./cmd/atlas
demo:
	python3 scripts/walkthrough.py --workflow submit-render --out var/demo

# Exchange-package import verification: adversarial validation, conflict
# atomicity, parallel imports, disk-write fault injection and render parity.
verify-import:
	go test -race -count=1 ./internal/bundle/ ./internal/persist/ ./internal/service/ ./internal/httpapi/
