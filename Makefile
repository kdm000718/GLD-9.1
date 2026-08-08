export GOTOOLCHAIN := local

build:
	go build ./...

test:
	go test -race ./...

vet:
	go vet ./...

# ---- 게이트 ----
# 포팅은 조용히 틀린다. 단계마다 Python 원본과 대조한다.

# G1 — 피처 동등성. 골든 벡터 2,997시점(k=0 2,000 + k=1~4 997)을 1e-9 로 대조하고,
# Python 이 거부한 3개 시점을 Go 도 거부하는지 함께 본다.
goldencheck:
	go run ./cmd/goldencheck

# G1' — 9년 전 구간 재현. 약 3분. 다섯 겹으로 판정한다:
# 절단 봉 수 / 제외 항목별 개수 / candle_start·라벨 원소별 일치 /
# Go 지표를 Python 확률값에 적용한 대조 / 표본별 확률 차이와 판정 뒤집힘.
# data/reference/py_predictions_full.bin 이 필요하다 (README 참고).
backtest:
	go run ./cmd/backtest

# G2 게이트. 기본 7일 — 2,669 회차로 불일치율 표준오차 0.49%p 라 판정에 충분하고,
# 수집기를 멈춰야 하는 시간이 짧다. 더 긴 구간은 `make align DAYS=30`.
DAYS ?= 7

align:
	go run ./cmd/align -days $(DAYS)

# 게이트 셋을 순서대로. G2 는 PREDICT_API_KEY 가 필요하므로 뺐다.
gates: goldencheck backtest

.PHONY: build test vet goldencheck backtest align gates
