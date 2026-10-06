package quota

import "testing"

func i64(v int64) *int64 { return &v }

func TestAdvanceRetainsUnknownAndNonAuthoritativeAlarms(t *testing.T) {
	q := i64(100)
	if streak, conf := Advance(2, true, false, false, nil, 1); streak != 2 || !conf {
		t.Fatalf("unknown threshold must not clear a confirmed alarm, got (%d,%v)", streak, conf)
	}
	if streak, conf := Advance(2, true, false, true, q, 1); streak != 2 || !conf {
		t.Fatalf("cached under-limit sample must not clear a confirmed alarm, got (%d,%v)", streak, conf)
	}
	if streak, conf := Advance(2, true, false, true, q, 200); streak != 3 || !conf {
		t.Fatalf("cached over sample should advance, got (%d,%v)", streak, conf)
	}
	if streak, conf := Advance(2, true, true, true, nil, 200); streak != 0 || conf {
		t.Fatalf("authoritative quota-off must clear, got (%d,%v)", streak, conf)
	}
	if streak, conf := Advance(2, true, true, true, q, 50); streak != 0 || conf {
		t.Fatalf("authoritative under-limit must clear, got (%d,%v)", streak, conf)
	}
}

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
		{"first equal sample counts as over", 0, i64(100), 100, 1, false},
		{"equal to quota confirms on second sample", 1, i64(100), 100, 2, true},
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
