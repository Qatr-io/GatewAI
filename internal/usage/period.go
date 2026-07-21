package usage

import (
	"fmt"
	"time"
)

// periodDaily, periodWeekly, periodMonthly are the supported report periods
// for GetUsageReport and the aggregate keys written by trackPeriodAggregate.
const (
	periodDaily   = "daily"
	periodWeekly  = "weekly"
	periodMonthly = "monthly"
)

// periods lists all supported report periods, in a stable order.
var periods = []string{periodDaily, periodWeekly, periodMonthly}

// periodBuckets returns the calendar-aligned bucket ID for t (converted to
// UTC) for every supported period: "20060102" (daily), ISO "2006-W02"
// (weekly), "200601" (monthly).
func periodBuckets(t time.Time) map[string]string {
	t = t.UTC()
	year, week := t.ISOWeek()
	return map[string]string{
		periodDaily:   t.Format("20060102"),
		periodWeekly:  fmt.Sprintf("%04d-W%02d", year, week),
		periodMonthly: t.Format("200601"),
	}
}

// periodTTL returns how long an aggregate key for the given period is kept
// before expiry. Daily and weekly buckets expire (bounded key growth);
// monthly buckets are kept indefinitely since their cardinality
// (service types × metrics × months) stays trivial for any realistic
// deployment lifetime.
func periodTTL(period string) time.Duration {
	switch period {
	case periodDaily:
		return 400 * 24 * time.Hour
	case periodWeekly:
		return 800 * 24 * time.Hour
	default:
		return 0
	}
}

// bucketID returns the bucket ID for t under the given period.
func bucketID(t time.Time, period string) string {
	return periodBuckets(t)[period]
}

// estimatedBucketCount returns an upper bound on the number of buckets a
// walkBuckets call for [from, to] would produce, cheap enough to check
// before doing the actual walk (bounds abusive ranges without building a
// huge slice first).
func estimatedBucketCount(period string, from, to time.Time) int {
	days := int(to.UTC().Sub(from.UTC()).Hours()/24) + 1
	if days < 1 {
		return 0
	}
	switch period {
	case periodWeekly:
		return days/7 + 2
	case periodMonthly:
		return days/28 + 2
	default:
		return days
	}
}

// walkBuckets returns the ordered, deduplicated bucket IDs covering [from, to]
// (inclusive) for the given period. Monthly buckets are walked by calendar
// month (normalized to day 1) to avoid day-of-month overflow skipping a
// short month (e.g. Jan 31 + 1 month landing in March).
func walkBuckets(period string, from, to time.Time) []string {
	from, to = from.UTC(), to.UTC()
	if to.Before(from) {
		return nil
	}

	var buckets []string
	switch period {
	case periodWeekly:
		last := ""
		for t := from; !t.After(to); t = t.AddDate(0, 0, 1) {
			if b := bucketID(t, period); b != last {
				buckets = append(buckets, b)
				last = b
			}
		}
	case periodMonthly:
		y, m, _ := from.Date()
		cur := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		endY, endM, _ := to.Date()
		end := time.Date(endY, endM, 1, 0, 0, 0, 0, time.UTC)
		for !cur.After(end) {
			buckets = append(buckets, cur.Format("200601"))
			cur = cur.AddDate(0, 1, 0)
		}
	default: // daily
		for t := from; !t.After(to); t = t.AddDate(0, 0, 1) {
			buckets = append(buckets, bucketID(t, period))
		}
	}
	return buckets
}

func isValidPeriod(period string) bool {
	for _, p := range periods {
		if p == period {
			return true
		}
	}
	return false
}
