.PHONY: check compatibility compatibility-current compatibility-minimum fmt integration verify

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

verify:
	go run ./internal/corecompat -line=minimum
	go run ./internal/corecompat -line=current
	go run ./internal/qualitygate -mode=verify
