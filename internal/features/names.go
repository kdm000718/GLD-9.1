package features

import "fmt"

// FeatureNames 는 피처 벡터의 순서를 정의하는 유일한 근거다.
// Python btc5m/features.py 의 FEATURE_NAMES 와 원소·순서가 완전히 같아야 한다.
var FeatureNames = buildFeatureNames()

func buildFeatureNames() []string {
	var names []string
	for _, tf := range []struct {
		prefix string
		lags   []int
	}{
		{"m1", []int{1, 3, 5, 15, 30, 60}},
		{"m5", []int{1, 2, 3, 6, 12}},
	} {
		for _, l := range tf.lags {
			names = append(names, fmt.Sprintf("%s_ret%d", tf.prefix, l))
		}
		for _, suffix := range []string{
			"rsi", "rsi_slope", "ema_spread", "atr_pct", "dist_ema_atr",
			"range_pos", "body", "vol", "vol_z", "streak",
			"div_reg_bear", "div_reg_bull", "div_hid_bear", "div_hid_bull", "div_score",
		} {
			names = append(names, tf.prefix+"_"+suffix)
		}
	}
	return append(names,
		"m1_taker_ratio5", "m1_taker_ratio15", "m1_taker_ratio60", "m5_taker_ratio6",
		"m1_upbar_ratio15", "m1_vwap_dev30", "m1_trades_z30", "m1_vol_ratio",
		"tod_sin", "tod_cos",
		// 진행 중인 5분봉의 마감된 부분 (k=0 이면 전부 0)
		"p_elapsed", "p_ret_from_open", "p_body_atr", "p_range_pos",
		"p_high_ext_atr", "p_low_ext_atr", "p_vol_frac", "p_taker_ratio", "p_up_min_frac",
	)
}

// index 는 이름 → 위치. Build 가 값을 채울 때 쓴다.
var index = func() map[string]int {
	m := make(map[string]int, len(FeatureNames))
	for i, n := range FeatureNames {
		m[n] = i
	}
	return m
}()
