package http

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

// activitySnapshot covers every state the Activity cell can render: moved,
// idle (ECS republished, nothing moved), unrefreshed (ECS provably has not
// republished), unchanged (ECS omits uptodate_till so the two are
// indistinguishable), and unknown (no baseline yet).
func activitySnapshot() poller.Snapshot {
	q := int64(1073741824)
	pct := 12.0
	ut := time.Date(2026, 10, 5, 14, 45, 0, 0, time.UTC)
	return poller.Snapshot{
		Namespace:      "prod-ns",
		PolledAt:       time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC),
		PollOK:         true,
		NamespaceBytes: 4096,
		ActivityWindow: 24 * time.Hour,
		Buckets: []poller.BucketState{
			{
				Name: "hot-bkt", Namespace: "prod-ns", UsedBytes: 2048, Objects: 500,
				QuotaBytes: &q, UsedPercent: &pct, UptodateTill: ut,
				Activity: poller.Activity{
					Known: true, Changed: true, MeteringRefreshed: true, FreshnessKnown: true,
					WindowSeconds: 900, ObjectsDelta: 60, BytesDelta: 1024,
					ObjectsPerHour: 240, BytesPerHour: 4096,
					BusyHours: 20, ObservedHours: 24, Hot: true,
				},
			},
			{
				Name: "quiet-bkt", Namespace: "prod-ns", UsedBytes: 1024, Objects: 10,
				UptodateTill: ut,
				Activity: poller.Activity{
					Known: true, MeteringRefreshed: true, FreshnessKnown: true,
					WindowSeconds: 900, BusyHours: 0, ObservedHours: 24,
				},
			},
			{
				Name: "mpu-bkt", Namespace: "prod-ns", UsedBytes: 512, Objects: 3, MPUParts: 7,
				UptodateTill: ut,
				Activity: poller.Activity{
					Known: true, Changed: true, MeteringRefreshed: true, FreshnessKnown: true,
					WindowSeconds: 900, ObjectsPerHour: 4, BytesPerHour: 128,
					MPUParts: 7, MPUPartsDelta: 7, BusyHours: 2, ObservedHours: 24,
				},
			},
			{
				Name: "stale-metering-bkt", Namespace: "prod-ns", UsedBytes: 256, Objects: 1,
				UptodateTill: ut,
				Activity: poller.Activity{
					Known: true, FreshnessKnown: true, WindowSeconds: 900,
					ObjectsPerHour: 12, BytesPerHour: 64,
				},
			},
			{
				// The common real-world case: ECS reports no uptodate_till for
				// this bucket, so idle and unrefreshed look identical.
				Name: "no-freshness-bkt", Namespace: "prod-ns", UsedBytes: 128, Objects: 1,
				Activity: poller.Activity{
					Known: true, WindowSeconds: 900, BusyHours: 1, ObservedHours: 9,
				},
			},
			{
				Name: "new-bkt", Namespace: "prod-ns", UsedBytes: 128, Objects: 1, UptodateTill: ut,
			},
		},
	}
}

func TestAPIBucketsCarriesDerivedActivity(t *testing.T) {
	s := newTestServer(t, activitySnapshot(), store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/buckets", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	var body struct {
		ActivityWindowSeconds int64 `json:"activity_window_seconds"`
		HotBuckets            int   `json:"hot_buckets"`
		Buckets               []struct {
			Name         string       `json:"name"`
			MPUParts     int64        `json:"mpu_parts"`
			UptodateTill *string      `json:"uptodate_till"`
			Activity     *apiActivity `json:"activity"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if body.ActivityWindowSeconds != 86400 {
		t.Errorf("activity_window_seconds = %d, want 86400", body.ActivityWindowSeconds)
	}
	if body.HotBuckets != 1 {
		t.Errorf("hot_buckets = %d, want 1", body.HotBuckets)
	}
	byName := map[string]int{}
	for i, b := range body.Buckets {
		byName[b.Name] = i
	}
	act := func(name string) *apiActivity {
		t.Helper()
		a := body.Buckets[byName[name]].Activity
		if a == nil {
			t.Fatalf("%s activity is null", name)
		}
		return a
	}

	hot := act("hot-bkt")
	if hot.State != "moved" || !hot.Changed || !hot.MeteringRefreshed || !hot.FreshnessKnown {
		t.Errorf("hot-bkt state = %+v, want a proven, changed observation", hot)
	}
	if hot.ObjectsPerHour != 240 || hot.BytesPerHour != 4096 {
		t.Errorf("rates = %v obj/h, %v B/h; want 240 / 4096", hot.ObjectsPerHour, hot.BytesPerHour)
	}
	if !hot.Hot || hot.BusyHours != 20 || hot.ObservedHours != 24 {
		t.Errorf("window = %d busy / %d observed, hot=%v; want 20 / 24 and hot",
			hot.BusyHours, hot.ObservedHours, hot.Hot)
	}
	if hot.WindowSeconds != 900 {
		t.Errorf("window_seconds = %v, want 900", hot.WindowSeconds)
	}
	if ut := body.Buckets[byName["hot-bkt"]].UptodateTill; ut == nil || !strings.Contains(*ut, "2026-10-05T14:45") {
		t.Errorf("uptodate_till = %v, want the ECS metering timestamp", ut)
	}

	if got := act("quiet-bkt").State; got != "idle" {
		t.Errorf("quiet-bkt state = %q, want idle (ECS republished, nothing moved)", got)
	}
	if got := act("stale-metering-bkt").State; got != "unrefreshed" {
		t.Errorf("stale-metering-bkt state = %q, want unrefreshed", got)
	}
	noFresh := act("no-freshness-bkt")
	if noFresh.State != "unchanged" || noFresh.FreshnessKnown || noFresh.MeteringRefreshed {
		t.Errorf("no-freshness-bkt = %+v, want state unchanged with unknown freshness", noFresh)
	}
	if noFresh.BusyHours != 1 || noFresh.ObservedHours != 9 {
		t.Errorf("no-freshness-bkt window = %d/%d, want 1/9 (measured hours still count)",
			noFresh.BusyHours, noFresh.ObservedHours)
	}
	if ut := body.Buckets[byName["no-freshness-bkt"]].UptodateTill; ut != nil {
		t.Errorf("uptodate_till = %v, want null when ECS omits it", ut)
	}

	mpu := body.Buckets[byName["mpu-bkt"]]
	if mpu.MPUParts != 7 {
		t.Errorf("mpu_parts = %d, want 7", mpu.MPUParts)
	}
	if mpu.Activity == nil || mpu.Activity.MPUPartsDelta != 7 {
		t.Errorf("mpu delta = %+v, want +7", mpu.Activity)
	}

	// No comparable baseline yet: null, not zeroes, so a consumer cannot read
	// a brand-new bucket as idle.
	if got := body.Buckets[byName["new-bkt"]].Activity; got != nil {
		t.Errorf("new-bkt activity = %+v, want null", got)
	}
}

func TestDashboardRendersActivityColumns(t *testing.T) {
	s := newTestServer(t, activitySnapshot(), store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()

	for _, want := range []string{
		"data-key=\"activity\"",      // sortable Activity column
		"data-key=\"busy\"",          // sortable Busy column
		"Busy 24h",                   // window label in the header
		"always busy",                // the "always used" badge
		"7 mpu parts",                // in-flight multipart upload
		"20/24h",                     // busy hours over measured hours
		"1/9h",                       // a bucket ECS gives no uptodate_till for
		"data-activity=\"240.0000\"", // sort key
		"data-busy=\"20\"",           // sort key
		"read traffic is invisible",  // the honesty caveat
		"colspan=\"10\"",             // empty/no-match rows span the new width
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// html/template escapes "+" as "&#43;" in text context: it renders
	// correctly in a browser but breaks literal assertions on signed rates.
	plain := strings.ReplaceAll(body, "&#43;", "+")
	rowsHTML := plain
	if start := strings.Index(plain, "<tbody"); start >= 0 {
		if end := strings.Index(plain[start:], "</tbody>"); end >= 0 {
			rowsHTML = plain[start : start+end]
		}
	}
	for _, want := range []string{"+240 obj/h", "+4.00 KiB/h"} {
		if !strings.Contains(rowsHTML, want) {
			t.Errorf("dashboard missing signed rate %q", want)
		}
	}
	// Each cell text must appear exactly once: only the two moved buckets
	// render a rate, and the three non-moving states stay distinguishable
	// instead of collapsing into one another. Counted inside <tbody> because
	// the footer quotes some of the same strings when it explains them.
	for want, count := range map[string]int{
		"obj/h":              2, // hot-bkt, mpu-bkt
		"idle":               1, // quiet-bkt: ECS republished and nothing moved
		"no new metering":    1, // stale-metering-bkt: provably unrefreshed
		"no change observed": 1, // no-freshness-bkt: ECS omits uptodate_till
		"always busy":        1, // hot-bkt only
	} {
		if got := strings.Count(rowsHTML, want); got != count {
			t.Errorf("rendered %d %q cells, want %d", got, want, count)
		}
	}
}

func TestWallboardRanksBusiestFirst(t *testing.T) {
	s := newTestServer(t, activitySnapshot(), store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/wallboard", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()

	for _, want := range []string{
		"class=\"busiest\"",
		"Busiest 24h",
		"Always busy 24h",
		"hot-bkt",
		"Reads are not counted by ECS billing",
		"no change observed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("wallboard missing %q", want)
		}
	}
	strip := body[strings.Index(body, "class=\"busiest\""):]
	if !strings.Contains(strip, "hot-bkt") {
		t.Error("busiest strip dropped the always-busy bucket")
	}
	if strings.Index(strip, "hot-bkt") > strings.Index(strip, "mpu-bkt") {
		t.Error("always-busy bucket must rank above a one-off spike")
	}
}

func TestBusiestBucketsOrdering(t *testing.T) {
	mk := func(name string, busy, observed int, objPerHour float64) bucketView {
		return bucketView{
			Name: name,
			Activity: activityView{
				Known: true, Changed: true, State: "moved",
				ObjectsPerHour: objPerHour, BytesPerHour: objPerHour,
				BusyHours: busy, ObservedHours: observed,
				Score:     math.Abs(objPerHour),
				BusyScore: busy,
				Busy:      "1/1h",
			},
		}
	}
	// unmeasured has the -1 sentinels toActivityView produces.
	unmeasured := bucketView{Name: "unmeasured", Activity: activityView{Score: -1, BusyScore: -1}}

	got := busiestBuckets([]bucketView{
		unmeasured,
		mk("spike", 1, 24, 9000),
		mk("steady", 20, 24, 5),
		mk("same-busy-lower-rate", 20, 24, 1),
	}, 3)

	if len(got) != 3 {
		t.Fatalf("returned %d entries, want the top 3: %+v", len(got), got)
	}
	order := []string{got[0].Name, got[1].Name, got[2].Name}
	want := []string{"steady", "same-busy-lower-rate", "spike"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v (measured hours first, then rate)", order, want)
		}
	}
}

func TestBusiestBucketsSkipsUnmeasured(t *testing.T) {
	got := busiestBuckets([]bucketView{
		{Name: "a", Activity: activityView{Score: -1, BusyScore: -1}},
	}, 5)
	if len(got) != 0 {
		t.Fatalf("unmeasured buckets must not appear as zero activity: %+v", got)
	}
}

func TestMeteringMarkerIsDisplayed(t *testing.T) {
	snap := activitySnapshot()
	snap.NamespaceUptodateTill = time.Date(2026, 10, 14, 4, 13, 30, 0, time.UTC)
	s := newTestServer(t, snap, store.NewMem(), "")

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	dash := rr.Body.String()
	for _, want := range []string{"Metering current to", "2026-10-14 04:13 UTC", "ago)"} {
		if !strings.Contains(dash, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// The footer must tie the derived rates to the marker's lag, since a rate
	// says when this tool observed a change, not when it happened.
	if !strings.Contains(dash, "not when the activity happened") {
		t.Error("dashboard footer does not explain what the metering lag means for rates")
	}

	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/wallboard", nil))
	if !strings.Contains(rr.Body.String(), "Metering current to") {
		t.Error("wallboard missing the metering currency stat")
	}

	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/buckets", nil))
	var body struct {
		NamespaceUptodateTill       *string `json:"namespace_uptodate_till"`
		NamespaceMeteringAgeSeconds *int64  `json:"namespace_metering_age_seconds"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.NamespaceUptodateTill == nil || !strings.Contains(*body.NamespaceUptodateTill, "2026-10-14T04:13:30Z") {
		t.Errorf("namespace_uptodate_till = %v, want the ECS marker", body.NamespaceUptodateTill)
	}
	if body.NamespaceMeteringAgeSeconds == nil || *body.NamespaceMeteringAgeSeconds <= 0 {
		t.Errorf("namespace_metering_age_seconds = %v, want a positive age", body.NamespaceMeteringAgeSeconds)
	}
}

func TestMeteringMarkerAbsentRendersNothing(t *testing.T) {
	// activitySnapshot leaves NamespaceUptodateTill zero, as ECS does when it
	// omits the field: nothing may render, and the API must say null rather
	// than invent an epoch timestamp.
	s := newTestServer(t, activitySnapshot(), store.NewMem(), "")
	for _, path := range []string{"/", "/wallboard"} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(rr.Body.String(), "Metering current to") {
			t.Errorf("%s rendered a metering marker ECS never sent", path)
		}
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/buckets", nil))
	var body struct {
		NamespaceUptodateTill       *string `json:"namespace_uptodate_till"`
		NamespaceMeteringAgeSeconds *int64  `json:"namespace_metering_age_seconds"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.NamespaceUptodateTill != nil || body.NamespaceMeteringAgeSeconds != nil {
		t.Errorf("absent marker must serialize as null, got %v / %v",
			body.NamespaceUptodateTill, body.NamespaceMeteringAgeSeconds)
	}
}

func TestActivityFormatting(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "0.0 obj/h"},
		{0.5, "+0.5 obj/h"},
		{-0.5, "-0.5 obj/h"},
		{9.25, "+9.2 obj/h"},
		{12, "+12 obj/h"},
		{240, "+240 obj/h"},
		{-240, "-240 obj/h"},
		{1234, "+1.2k obj/h"},
		{1234567, "+1.23M obj/h"},
		{4.5e9, "+4.50G obj/h"},
	} {
		if got := humanObjectsRate(tc.in); got != tc.want {
			t.Errorf("humanObjectsRate(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}

	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "0 B/h"},
		{512, "+512 B/h"},
		{4096, "+4.00 KiB/h"},
		{-4096, "-4.00 KiB/h"},
		{3 * 1024 * 1024 * 1024, "+3.00 GiB/h"},
	} {
		if got := humanBytesRate(tc.in); got != tc.want {
			t.Errorf("humanBytesRate(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}

	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "—"},
		{24 * time.Hour, "24h"},
		{90 * time.Minute, "90m"},
		{45 * time.Second, "45s"},
		{2*time.Hour + 30*time.Minute, "150m"},
	} {
		if got := shortDuration(tc.in); got != tc.want {
			t.Errorf("shortDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestActivityStateLabels(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   poller.Activity
		want string
	}{
		{"no baseline", poller.Activity{}, "unknown"},
		{"moved", poller.Activity{Known: true, Changed: true}, "moved"},
		{"moved without proof of refresh", poller.Activity{Known: true, Changed: true, FreshnessKnown: true}, "moved"},
		{"provably idle", poller.Activity{Known: true, MeteringRefreshed: true, FreshnessKnown: true}, "idle"},
		{"provably unrefreshed", poller.Activity{Known: true, FreshnessKnown: true}, "unrefreshed"},
		{"no uptodate_till", poller.Activity{Known: true}, "unchanged"},
	} {
		if got := activityState(tc.in); got != tc.want {
			t.Errorf("%s: activityState = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestToActivityViewSentinels(t *testing.T) {
	unknown := toActivityView(poller.Activity{}, 24*time.Hour)
	if unknown.Known || unknown.Score != -1 || unknown.BusyScore != -1 {
		t.Fatalf("unknown activity must sort last and render a dash: %+v", unknown)
	}
	if unknown.State != "unknown" {
		t.Errorf("state = %q, want unknown", unknown.State)
	}
	if unknown.ObjectsRate != "—" || unknown.BytesRate != "—" || unknown.Busy != "—" {
		t.Fatalf("unknown activity rendered fabricated numbers: %+v", unknown)
	}
	if unknown.Window != "24h" {
		t.Errorf("window label = %q, want 24h", unknown.Window)
	}

	measured := toActivityView(poller.Activity{
		Known: true, Changed: true, ObjectsPerHour: -12,
		BusyHours: 3, ObservedHours: 5,
	}, 24*time.Hour)
	if measured.State != "moved" {
		t.Errorf("state = %q, want moved", measured.State)
	}
	if measured.Score != 12 {
		t.Errorf("score = %v, want the absolute rate 12", measured.Score)
	}
	if measured.Busy != "3/5h" {
		t.Errorf("busy = %q, want 3/5h (busy over measured, not over the window)", measured.Busy)
	}
	if measured.ObjectsRate != "-12 obj/h" {
		t.Errorf("rate = %q, want -12 obj/h", measured.ObjectsRate)
	}
}
