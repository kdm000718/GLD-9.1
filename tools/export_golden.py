#!/usr/bin/env python3
"""Go 포팅 대조용 골든 벡터를 내보낸다.

btc5m_prediction_agent 의 피처 파이프라인으로 9년 구간에서 N개 시점을 뽑아
(candle_start, t, 피처 60개) 를 JSONL 로 쓴다. 시드가 고정이라 재현 가능하다.

두 블록을 쓴다.
  1) k=0 블록 — 의사결정 시각 t == candle_start. 실거래·Task 12 가 쓰는 유일한 경로.
  2) k>=1 블록 — t = candle_start + k분 (k = 1..4). Go 포팅에서는 도달하지 않는
     분기지만 Python backtest.py 의 offset_min 이 실제로 쓰는 경로라, 대조 없이
     두면 나중에 켤 때 조용히 틀린다. p_* 9개 피처의 실제 계산이 여기서만 실행된다.
"""

import argparse
import json
import random
import sys

import numpy as np

from btc5m import features as ft
from btc5m import vision
from btc5m.clock import LookaheadError, MarketView

SEED = 20260808
SEED_PARTIAL = 20260809


def emit(fh, b1, b5, picks, k_of, stats):
    """picks 의 각 인덱스에서 피처를 만들어 한 줄씩 쓴다."""
    for i in picks:
        cs = int(b5.open_time[i])
        k = k_of(i)
        t = cs + k * 60_000
        try:
            built = ft.build(MarketView(t, b1, b5, candle_start=cs))
        except LookaheadError:
            stats["skipped"] += 1
            continue
        if built is None:
            # 거부도 기록한다. Python 이 거부하는 시점을 Go 가 받아들이면
            # (또는 그 반대면) 결측 가드가 어긋난 것이고, 그 불일치는
            # 값 대조만으로는 절대 드러나지 않는다.
            fh.write(json.dumps({
                "candle_start": cs, "t": t, "k": k, "rejected": True,
            }) + "\n")
            stats["rejected"] += 1
            continue
        fh.write(json.dumps({
            "candle_start": cs,
            "t": t,
            "k": k,
            "values": [float(x) for x in built[0]],
        }) + "\n")
        stats["written"] += 1
        stats["by_k"][k] = stats["by_k"].get(k, 0) + 1


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--n", type=int, default=2000, help="k=0 표본 수")
    ap.add_argument("--n-partial", type=int, default=1000, help="k>=1 표본 수")
    args = ap.parse_args()

    b1 = vision.load_full_history("BTCUSDT", "1m", log=lambda *_: None)
    b5 = vision.load_full_history("BTCUSDT", "5m", log=lambda *_: None)
    print(f"1분봉 {len(b1):,} / 5분봉 {len(b5):,}", file=sys.stderr)

    # 워밍업 30일을 건너뛴 뒤부터 뽑는다.
    lo = int(np.searchsorted(b5.open_time, int(b5.open_time[0]) + 30 * 86_400_000))
    stats = {"written": 0, "skipped": 0, "rejected": 0, "by_k": {}}

    with open(args.out, "w") as fh:
        # 블록 1 — k=0. 시드와 추출 방식을 이전 판과 동일하게 유지한다.
        rng = random.Random(SEED)
        picks0 = sorted(rng.sample(range(lo, len(b5)), args.n))
        emit(fh, b1, b5, picks0, lambda _i: 0, stats)

        # 블록 2 — k>=1. 독립 시드라 블록 1 의 표본 집합이 바뀌지 않는다.
        rngp = random.Random(SEED_PARTIAL)
        picksp = sorted(rngp.sample(range(lo, len(b5)), args.n_partial))
        kmap = {i: rngp.randint(1, 4) for i in picksp}
        emit(fh, b1, b5, picksp, lambda i: kmap[i], stats)

    by_k = ", ".join(f"k={k}:{n}" for k, n in sorted(stats["by_k"].items()))
    print(f"기록 {stats['written']}개 ({by_k}), 거부기록 {stats['rejected']}개, 제외 {stats['skipped']}개 → {args.out}",
          file=sys.stderr)
    print(f"피처 이름 {len(ft.FEATURE_NAMES)}개", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
