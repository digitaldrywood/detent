package activehours

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const lookaheadDays = 15

type Config struct {
	Timezone string   `yaml:"timezone"`
	Windows  []string `yaml:"windows"`
}

type Status struct {
	Configured     bool
	WindowOpen     bool
	Open           bool
	OverrideActive bool
	Timezone       string
	WindowStart    time.Time
	NextOpen       time.Time
	NextClose      time.Time
	OverrideUntil  time.Time
}

type window struct {
	days        [7]bool
	startMinute int
	endMinute   int
}

type interval struct {
	start time.Time
	end   time.Time
}

func (c Config) IsZero() bool {
	return strings.TrimSpace(c.Timezone) == "" && len(c.Windows) == 0
}

func (c Config) Configured() bool {
	return !c.IsZero()
}

func (c Config) Normalize() Config {
	c.Timezone = strings.TrimSpace(c.Timezone)
	windows := make([]string, 0, len(c.Windows))
	for _, value := range c.Windows {
		value = strings.TrimSpace(value)
		if value != "" {
			windows = append(windows, value)
		}
	}
	c.Windows = windows
	return c
}

func (c Config) Validate(prefix string) []string {
	c = c.Normalize()
	if c.IsZero() {
		return nil
	}
	if prefix == "" {
		prefix = "active_hours"
	}

	var problems []string
	if c.Timezone == "" {
		problems = append(problems, prefix+".timezone: is required when active_hours is set")
	} else if _, err := time.LoadLocation(c.Timezone); err != nil {
		problems = append(problems, prefix+".timezone: must be a valid IANA timezone")
	}
	if len(c.Windows) == 0 {
		problems = append(problems, prefix+".windows: must contain at least one recurring window")
	}
	for index, value := range c.Windows {
		if _, err := parseWindow(value); err != nil {
			problems = append(problems, fmt.Sprintf("%s.windows[%d]: %v", prefix, index, err))
		}
	}
	return problems
}

func ParsePersistedOverride(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func Evaluate(c Config, now, overrideUntil time.Time) (Status, error) {
	c = c.Normalize()
	status := Status{Configured: c.Configured(), Timezone: c.Timezone, OverrideUntil: overrideUntil}
	if !status.Configured {
		status.Open = true
		return status, nil
	}
	if problems := c.Validate("active_hours"); len(problems) > 0 {
		return Status{}, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	location, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return Status{}, fmt.Errorf("load active-hours timezone %q: %w", c.Timezone, err)
	}
	windows := make([]window, 0, len(c.Windows))
	for _, value := range c.Windows {
		parsed, err := parseWindow(value)
		if err != nil {
			return Status{}, err
		}
		windows = append(windows, parsed)
	}
	status.OverrideActive = !overrideUntil.IsZero() && now.Before(overrideUntil)
	if windowsAlwaysOpen(windows) {
		status.WindowOpen = true
		status.Open = true
		return status, nil
	}
	intervals := recurringIntervals(windows, now.In(location))
	for index, candidate := range intervals {
		if !now.Before(candidate.start) && now.Before(candidate.end) {
			status.WindowOpen = true
			status.WindowStart = candidate.start
			status.NextClose = candidate.end
			if index+1 < len(intervals) {
				status.NextOpen = intervals[index+1].start
			}
			break
		}
		if now.Before(candidate.start) {
			status.NextOpen = candidate.start
			status.NextClose = candidate.end
			break
		}
	}
	status.Open = status.WindowOpen || status.OverrideActive
	return status, nil
}

func windowsAlwaysOpen(windows []window) bool {
	const minutesPerWeek = 7 * 24 * 60
	covered := make([]bool, minutesPerWeek)
	for _, item := range windows {
		for day, enabled := range item.days {
			if !enabled {
				continue
			}
			duration := item.endMinute - item.startMinute
			if item.endMinute < item.startMinute {
				duration = 24*60 - item.startMinute + item.endMinute
			}
			start := day*24*60 + item.startMinute
			for offset := range duration {
				covered[(start+offset)%minutesPerWeek] = true
			}
		}
	}
	return !slices.Contains(covered, false)
}

func parseWindow(value string) (window, error) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) != 2 {
		return window{}, errors.New("must use <WEEKDAY-FROM>-<WEEKDAY-TO> <HH>:<MM>-<HH>:<MM>")
	}
	dayParts := strings.Split(fields[0], "-")
	if len(dayParts) != 2 {
		return window{}, errors.New("weekday range must use Mon-Sun form")
	}
	startDay, ok := parseWeekday(dayParts[0])
	if !ok {
		return window{}, fmt.Errorf("weekday %q must be one of Mon, Tue, Wed, Thu, Fri, Sat, Sun", dayParts[0])
	}
	endDay, ok := parseWeekday(dayParts[1])
	if !ok {
		return window{}, fmt.Errorf("weekday %q must be one of Mon, Tue, Wed, Thu, Fri, Sat, Sun", dayParts[1])
	}
	timeParts := strings.Split(fields[1], "-")
	if len(timeParts) != 2 {
		return window{}, errors.New("time range must use HH:MM-HH:MM form")
	}
	startMinute, err := parseClock(timeParts[0], false)
	if err != nil {
		return window{}, fmt.Errorf("start time: %w", err)
	}
	endMinute, err := parseClock(timeParts[1], true)
	if err != nil {
		return window{}, fmt.Errorf("end time: %w", err)
	}
	if startMinute == endMinute {
		return window{}, errors.New("start and end must differ; use 00:00-24:00 for a full day")
	}

	parsed := window{startMinute: startMinute, endMinute: endMinute}
	for day := startDay; ; day = (day + 1) % 7 {
		parsed.days[day] = true
		if day == endDay {
			break
		}
	}
	return parsed, nil
}

func parseWeekday(value string) (int, bool) {
	days := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	index := slices.Index(days, value)
	return index, index >= 0
}

func parseClock(value string, allowEndOfDay bool) (int, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, fmt.Errorf("%q must use zero-padded 24-hour HH:MM", value)
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil || minute < 0 || minute > 59 || hour < 0 || hour > 23 {
		if allowEndOfDay && hour == 24 && minute == 0 && hourErr == nil && minuteErr == nil {
			return 24 * 60, nil
		}
		return 0, fmt.Errorf("%q must be a valid 24-hour time", value)
	}
	return hour*60 + minute, nil
}

func recurringIntervals(windows []window, now time.Time) []interval {
	startDate := localMidnight(now).AddDate(0, 0, -1)
	intervals := make([]interval, 0, len(windows)*(lookaheadDays+2))
	for offset := 0; offset <= lookaheadDays+1; offset++ {
		date := startDate.AddDate(0, 0, offset)
		weekday := int(date.Weekday())
		for _, item := range windows {
			if !item.days[weekday] {
				continue
			}
			start := localTime(date, item.startMinute)
			endDate := date
			if item.endMinute <= item.startMinute || item.endMinute == 24*60 {
				endDate = endDate.AddDate(0, 0, 1)
			}
			endMinute := item.endMinute
			if endMinute == 24*60 {
				endMinute = 0
			}
			intervals = append(intervals, interval{start: start, end: localTime(endDate, endMinute)})
		}
	}
	return mergeIntervals(intervals)
}

func localMidnight(value time.Time) time.Time {
	year, month, day := value.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, value.Location())
}

func localTime(date time.Time, minute int) time.Time {
	year, month, day := date.Date()
	return time.Date(year, month, day, minute/60, minute%60, 0, 0, date.Location())
}

func mergeIntervals(intervals []interval) []interval {
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start.Equal(intervals[j].start) {
			return intervals[i].end.Before(intervals[j].end)
		}
		return intervals[i].start.Before(intervals[j].start)
	})
	merged := make([]interval, 0, len(intervals))
	for _, candidate := range intervals {
		if len(merged) == 0 || candidate.start.After(merged[len(merged)-1].end) {
			merged = append(merged, candidate)
			continue
		}
		if candidate.end.After(merged[len(merged)-1].end) {
			merged[len(merged)-1].end = candidate.end
		}
	}
	return merged
}
