package quota

import "testing"

func i64(v int64) *int64 { return &v }

func TestApply(t *testing.T) {
	cases := []struct {
		name     string
		streak   int
		quota    *int64
		used     int64
		wantStrk int
		wantConf bool
	}{
		{"no quota resets", 5, nil, 999, 0, false},
		{"first over sample", 0, i64(100), 200, 1, false},
		{"second over sample confirms", 1, i64(100), 200, 2, true},
		{"third over sample stays confirmed", 2, i64(100), 200, 3, true},
		{"under resets streak", 2, i64(100), 50, 0, false},
		{"equal to quota is not over", 1, i64(100), 100, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, conf := Apply(c.streak, c.quota, c.used)
			if s != c.wantStrk || conf != c.wantConf {
				t.Fatalf("Apply(%d,%v,%d) = (%d,%v), want (%d,%v)",
					c.streak, c.quota, c.used, s, conf, c.wantStrk, c.wantConf)
			}
		})
	}
}
