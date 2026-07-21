package usage

import (
	"testing"
	"time"
)

func TestPeriodBuckets_UTCConversion(t *testing.T) {
	// 2026-03-01 23:30 in UTC-5 is 2026-03-02 04:30 UTC — bucket IDs must use
	// the UTC date, not the local one.
	loc := time.FixedZone("UTC-5", -5*60*60)
	local := time.Date(2026, 3, 1, 23, 30, 0, 0, loc)

	buckets := periodBuckets(local)
	if buckets[periodDaily] != "20260302" {
		t.Errorf("daily = %q, want 20260302 (UTC date)", buckets[periodDaily])
	}
	if buckets[periodMonthly] != "202603" {
		t.Errorf("monthly = %q, want 202603", buckets[periodMonthly])
	}
}

func TestPeriodBuckets_DailyFormat(t *testing.T) {
	tm := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	if got := bucketID(tm, periodDaily); got != "20260105" {
		t.Errorf("got %q, want 20260105", got)
	}
}

func TestPeriodBuckets_MonthlyFormat(t *testing.T) {
	tm := time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)
	if got := bucketID(tm, periodMonthly); got != "202612" {
		t.Errorf("got %q, want 202612", got)
	}
}

func TestPeriodBuckets_ISOWeek_YearBoundary(t *testing.T) {
	// 2025-12-31 is ISO week 2026-W01 (year-end week rolls into next ISO year).
	tm := time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)
	got := bucketID(tm, periodWeekly)
	wantYear, wantWeek := tm.ISOWeek()
	if wantYear != 2026 || wantWeek != 1 {
		t.Fatalf("test assumption wrong: ISOWeek() = %d, %d", wantYear, wantWeek)
	}
	if got != "2026-W01" {
		t.Errorf("got %q, want 2026-W01", got)
	}
}

func TestPeriodTTL_DailyAndWeeklySet_MonthlyZero(t *testing.T) {
	if periodTTL(periodDaily) != 400*24*time.Hour {
		t.Errorf("daily TTL = %v", periodTTL(periodDaily))
	}
	if periodTTL(periodWeekly) != 800*24*time.Hour {
		t.Errorf("weekly TTL = %v", periodTTL(periodWeekly))
	}
	if periodTTL(periodMonthly) != 0 {
		t.Errorf("monthly TTL = %v, want 0 (no expiry)", periodTTL(periodMonthly))
	}
}

func TestWalkBuckets_Daily_InclusiveRange(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)

	got := walkBuckets(periodDaily, from, to)
	want := []string{"20260301", "20260302", "20260303"}
	if !equalSlices(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkBuckets_Weekly_Deduplicates(t *testing.T) {
	// A 10-day range spans parts of 2-3 ISO weeks; each bucket ID must appear
	// only once even though multiple days fall in the same week.
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC) // Sunday
	to := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)  // Tuesday, 10 days later

	got := walkBuckets(periodWeekly, from, to)
	seen := map[string]bool{}
	for _, b := range got {
		if seen[b] {
			t.Fatalf("duplicate bucket %q in %v", b, got)
		}
		seen[b] = true
	}
	if len(got) < 2 {
		t.Errorf("expected at least 2 distinct week buckets for a 10-day span, got %v", got)
	}
}

func TestWalkBuckets_Monthly_NoDayOverflowSkip(t *testing.T) {
	// Regression: from=Jan 31 must not skip February when walking by month.
	// Naively calling AddDate(0, 1, 0) on Jan 31 repeatedly overflows into
	// March (Go's day-overflow semantics), silently dropping the Feb bucket.
	from := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)

	got := walkBuckets(periodMonthly, from, to)
	want := []string{"202601", "202602", "202603"}
	if !equalSlices(got, want) {
		t.Errorf("got %v, want %v (February bucket must not be skipped)", got, want)
	}
}

func TestWalkBuckets_SingleDay(t *testing.T) {
	d := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	for _, period := range periods {
		got := walkBuckets(period, d, d)
		if len(got) != 1 {
			t.Errorf("period=%s: expected 1 bucket for a single-day range, got %v", period, got)
		}
	}
}

func TestWalkBuckets_ToBeforeFrom_ReturnsNil(t *testing.T) {
	from := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	if got := walkBuckets(periodDaily, from, to); got != nil {
		t.Errorf("expected nil for to < from, got %v", got)
	}
}

func TestEstimatedBucketCount_Sanity(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC) // 365 days

	if n := estimatedBucketCount(periodDaily, from, to); n < 365 {
		t.Errorf("daily estimate = %d, want >= 365", n)
	}
	if n := estimatedBucketCount(periodWeekly, from, to); n < 52 {
		t.Errorf("weekly estimate = %d, want >= 52", n)
	}
	if n := estimatedBucketCount(periodMonthly, from, to); n < 12 {
		t.Errorf("monthly estimate = %d, want >= 12", n)
	}
}

func TestEstimatedBucketCount_ToBeforeFrom_Zero(t *testing.T) {
	from := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	if n := estimatedBucketCount(periodDaily, from, to); n != 0 {
		t.Errorf("got %d, want 0", n)
	}
}

func TestIsValidPeriod(t *testing.T) {
	for _, p := range []string{periodDaily, periodWeekly, periodMonthly} {
		if !isValidPeriod(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []string{"", "hourly", "yearly", "DAILY"} {
		if isValidPeriod(p) {
			t.Errorf("%q should be invalid", p)
		}
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
