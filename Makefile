.PHONY: check compatibility compatibility-current compatibility-minimum fmt integration release-parity verify verify-release

check:
	go run ./internal/qualitygate -mode=check

compatibility: compatibility-minimum compatibility-current

compatibility-current:
	go run ./internal/corecompat -line=current

compatibility-minimum:
	go run ./internal/corecompat -line=minimum

fmt:
	go run ./internal/qualitygate -mode=fmt

integration:
	go test -tags=integration -race -shuffle=on -count=1 ./...

release-parity: export GOWORK := off
release-parity: export GOPROXY := off
release-parity: export GOTOOLCHAIN := local
release-parity: export GOFLAGS := -mod=vendor
release-parity:
	go run ./internal/qualitygate -mode=release-parity

verify:
	go run ./internal/corecompat -line=minimum
	go run ./internal/corecompat -line=current
	go run ./internal/qualitygate -mode=verify

verify-release:
	go run ./internal/qualitygate -mode=verify-release
