#!/usr/bin/env python3
"""Python 참조 실행의 표본별 예측을 Go 가 읽을 수 있는 바이너리로 내보낸다.

G1' 게이트는 원래 요약 4개(표본 수·정확도·AUC·재학습 횟수)만 대조했다.
그것만으로는 약하다 — 정확도는 서로 다른 예측 집합에서도 우연히 같을 수 있고,
LBFGS 두 구현이 다른 점에 수렴해도 총계는 소수점 아래에서 만날 수 있다.
표본별로 맞추면 어긋난 지점을 시각으로 짚어낼 수 있다.

    cd /home/kdm00/kdm/btc5m_prediction_agent
    python3 /home/kdm00/kdm/GLD-9.1/tools/export_py_predictions.py \
        --npz out_full/predictions_full.npz \
        --out /home/kdm00/kdm/GLD-9.1/data/reference/py_predictions_full.bin

형식 (전부 little-endian, 열 단위로 이어 붙인다):
    magic   8바이트  "GLD9PRED"
    n       int64
    cs      int64   × n   candle_start (밀리초, 오름차순)
    prob    float64 × n   prob_up
    label   int8    × n   0 또는 1
"""

import argparse
import struct
import sys

import numpy as np

MAGIC = b"GLD9PRED"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--npz", required=True)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    d = np.load(args.npz)
    cs = np.ascontiguousarray(d["candle_start"], dtype="<i8")
    prob = np.ascontiguousarray(d["prob_up"], dtype="<f8")
    label = np.ascontiguousarray(d["label"], dtype=np.int8)
    n = len(cs)
    if not (len(prob) == len(label) == n):
        raise SystemExit("열 길이가 서로 다릅니다")
    if not np.all(np.diff(cs) > 0):
        raise SystemExit("candle_start 가 오름차순이 아닙니다")

    with open(args.out, "wb") as fh:
        fh.write(MAGIC)
        fh.write(struct.pack("<q", n))
        fh.write(cs.tobytes())
        fh.write(prob.tobytes())
        fh.write(label.tobytes())

    acc = ((prob >= 0.5).astype(np.int8) == label).mean() * 100
    print(f"표본 {n:,}개 → {args.out}", file=sys.stderr)
    print(f"  구간 {cs[0]} ~ {cs[-1]}", file=sys.stderr)
    print(f"  정확도 {acc:.3f}%  (참조값 52.773%)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
