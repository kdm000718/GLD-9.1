package rest

import "testing"

func TestParseSlugStart(t *testing.T) {
	cases := []struct {
		slug string
		want int64
		ok   bool
	}{
		{"btc-updown-5m-1786190100", 1786190100, true},
		{"eth-updown-5m-1786192500", 1786192500, true},
		{"btc-updown-15m-1786190100", 1786190100, true},
		{"btc-updown-5m-notanumber", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseSlugStart(c.slug)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseSlugStart(%q) = (%d, %v), 기대 (%d, %v)", c.slug, got, ok, c.want, c.ok)
		}
	}
}

func TestParseSlugStartRejectsUnaligned(t *testing.T) {
	// 5분 경계가 아닌 값은 회차 슬러그일 수 없다
	if _, ok := ParseSlugStart("btc-updown-5m-1786190123"); ok {
		t.Error("5분 경계가 아닌 슬러그를 받아들였다")
	}
}
