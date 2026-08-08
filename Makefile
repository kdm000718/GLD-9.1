export GOTOOLCHAIN := local

build:
	go build ./...

test:
	go test -race ./...

vet:
	go vet ./...

# G2 게이트. 기본 7일 — 2,669 회차로 불일치율 표준오차 0.49%p 라 판정에 충분하고,
# 수집기를 멈춰야 하는 시간이 짧다. 더 긴 구간은 `make align DAYS=30`.
DAYS ?= 7

align:
	go run ./cmd/align -days $(DAYS)
