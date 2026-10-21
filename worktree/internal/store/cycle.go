package store

import "time"

var cycleTimeBucketRanges = []CycleTimeBucket{
	{Label: "<1h", MinSeconds: 0, MaxSeconds: int64(time.Hour / time.Second)},
	{Label: "1-4h", MinSeconds: int64(time.Hour / time.Second), MaxSeconds: int64(4 * time.Hour / time.Second)},
	{Label: "4-8h", MinSeconds: int64(4 * time.Hour / time.Second), MaxSeconds: int64(8 * time.Hour / time.Second)},
	{Label: "8-24h", MinSeconds: int64(8 * time.Hour / time.Second), MaxSeconds: int64(24 * time.Hour / time.Second)},
	{Label: "1-3d", MinSeconds: int64(24 * time.Hour / time.Second), MaxSeconds: int64(72 * time.Hour / time.Second)},
	{Label: "3-7d", MinSeconds: int64(72 * time.Hour / time.Second), MaxSeconds: int64(7 * 24 * time.Hour / time.Second)},
	{Label: "7d+", MinSeconds: int64(7 * 24 * time.Hour / time.Second)},
}

func cycleTimeSeconds(start time.Time, end time.Time) (int64, bool) {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0, false
	}
	return int64(end.Sub(start) / time.Second), true
}

func cycleTimeBuckets(issues []CycleTimeIssue) []CycleTimeBucket {
	if len(issues) == 0 {
		return nil
	}

	buckets := append([]CycleTimeBucket(nil), cycleTimeBucketRanges...)
	lastNonZero := -1
	for _, issue := range issues {
		if issue.DurationSeconds < 0 {
			continue
		}
		for index := range buckets {
			bucket := buckets[index]
			if issue.DurationSeconds < bucket.MinSeconds {
				continue
			}
			if bucket.MaxSeconds > 0 && issue.DurationSeconds >= bucket.MaxSeconds {
				continue
			}
			buckets[index].Count++
			if index > lastNonZero {
				lastNonZero = index
			}
			break
		}
	}
	if lastNonZero < 0 {
		return nil
	}
	return buckets[:lastNonZero+1]
}

func averageCycleTimeSeconds(issues []CycleTimeIssue) int64 {
	if len(issues) == 0 {
		return 0
	}

	total := int64(0)
	for _, issue := range issues {
		total += issue.DurationSeconds
	}
	return total / int64(len(issues))
}
