.PHONY: check compatibility compatibility-current compatibility-minimum fmt integration release-rehearsal verify verify-release

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

release-rehearsal: export GOWORK := off
release-rehearsal: export GOPROXY := off
release-rehearsal: export GOTOOLCHAIN := local
release-rehearsal: export GOFLAGS := -mod=vendor
release-rehearsal:
	go run ./internal/qualitygate -mode=release-rehearsal

verify:
	go run ./internal/corecompat -line=minimum
	go run ./internal/corecompat -line=current
	go run ./internal/qualitygate -mode=verify

verify-release:
	go run ./internal/qualitygate -mode=verify-release
