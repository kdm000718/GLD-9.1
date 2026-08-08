package features

import "testing"

func TestFeatureNamesCountAndUniqueness(t *testing.T) {
	if len(FeatureNames) != 60 {
		t.Fatalf("피처 %d개, 기대 60개", len(FeatureNames))
	}
	seen := map[string]bool{}
	for i, n := range FeatureNames {
		if n == "" {
			t.Fatalf("%d번째 이름이 비어 있다", i)
		}
		if seen[n] {
			t.Fatalf("이름 중복: %s", n)
		}
		seen[n] = true
	}
}

// Python FEATURE_NAMES 와 순서까지 완전히 같아야 한다.
// 이 목록은 btc5m/features.py 의 _build_feature_names() 출력을 그대로 옮긴 것이다.
func TestFeatureNamesMatchPythonOrder(t *testing.T) {
	want := []string{
		"m1_ret1", "m1_ret3", "m1_ret5", "m1_ret15", "m1_ret30", "m1_ret60",
		"m1_rsi", "m1_rsi_slope", "m1_ema_spread", "m1_atr_pct", "m1_dist_ema_atr",
		"m1_range_pos", "m1_body", "m1_vol", "m1_vol_z", "m1_streak",
		"m1_div_reg_bear", "m1_div_reg_bull", "m1_div_hid_bear", "m1_div_hid_bull", "m1_div_score",
		"m5_ret1", "m5_ret2", "m5_ret3", "m5_ret6", "m5_ret12",
		"m5_rsi", "m5_rsi_slope", "m5_ema_spread", "m5_atr_pct", "m5_dist_ema_atr",
		"m5_range_pos", "m5_body", "m5_vol", "m5_vol_z", "m5_streak",
		"m5_div_reg_bear", "m5_div_reg_bull", "m5_div_hid_bear", "m5_div_hid_bull", "m5_div_score",
		"m1_taker_ratio5", "m1_taker_ratio15", "m1_taker_ratio60", "m5_taker_ratio6",
		"m1_upbar_ratio15", "m1_vwap_dev30", "m1_trades_z30", "m1_vol_ratio",
		"tod_sin", "tod_cos",
		"p_elapsed", "p_ret_from_open", "p_body_atr", "p_range_pos",
		"p_high_ext_atr", "p_low_ext_atr", "p_vol_frac", "p_taker_ratio", "p_up_min_frac",
	}
	if len(want) != len(FeatureNames) {
		t.Fatalf("길이 불일치: %d vs %d", len(FeatureNames), len(want))
	}
	for i := range want {
		if FeatureNames[i] != want[i] {
			t.Fatalf("%d번째: %q, 기대 %q", i, FeatureNames[i], want[i])
		}
	}
}
