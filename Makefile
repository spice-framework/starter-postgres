.PHONY: check fmt integration verify

check:
	go run ./internal/qualitygate -mode=check

fmt:
	go run ./internal/qualitygate -mode=fmt

integration:
	go test -tags=integration -race -shuffle=on -count=1 ./...

verify:
	go run ./internal/qualitygate -mode=verify
