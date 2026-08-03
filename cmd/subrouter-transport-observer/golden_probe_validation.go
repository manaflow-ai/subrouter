package main

import (
	"sort"
	"time"
)

func (s *goldenProbeStats) validateInterval(start, end time.Time) error {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return failGolden("health_interval_missing")
	}
	s.mu.Lock()
	events := append([]goldenProbeEvent(nil), s.events...)
	s.mu.Unlock()
	labels := []string{"public-health", "public-ready", "local-health", "local-ready"}
	for _, label := range labels {
		var stamps []time.Time
		for _, event := range events {
			stamp, _ := time.Parse(time.RFC3339Nano, event.Timestamp)
			if event.Label == label && !stamp.Before(start) && !stamp.After(end) {
				if !event.OK {
					return failGolden("health_probe_failed")
				}
				stamps = append(stamps, stamp)
			}
		}
		sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
		minimum := int(end.Sub(start) / goldenProbeInterval)
		if minimum > 2 {
			minimum--
		}
		if minimum < 2 {
			minimum = 2
		}
		if len(stamps) < minimum {
			return failGolden("health_probe_frequency_low")
		}
		maximumGap := goldenProbeInterval + goldenProbeScheduleTolerance
		if stamps[0].Sub(start) > maximumGap || end.Sub(stamps[len(stamps)-1]) > maximumGap {
			return failGolden("health_probe_coverage_gap")
		}
		for index := 1; index < len(stamps); index++ {
			if stamps[index].Sub(stamps[index-1]) > maximumGap {
				return failGolden("health_probe_gap")
			}
		}
	}
	return nil
}
