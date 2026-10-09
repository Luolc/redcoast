package claude

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Luolc/redcoast/session"
)

// parseUtilization is the one place the utilization header's scale is interpreted.
// The value is a fraction of the window's quota and is stored as read: a reading of
// 0.04 is what the client shows as 4%. It has no upper bound, since a window used past
// its quota may read above 1.
func parseUtilization(value string) (float64, bool) {
	fraction, err := strconv.ParseFloat(value, 64)
	// Stated positively so that NaN, which fails every comparison, is refused too.
	if err != nil || !(fraction >= 0 && fraction <= math.MaxFloat64) {
		return 0, false
	}
	return fraction, true
}

// quotaReadings parses the anthropic-ratelimit-unified-{5h,7d}-* headers of an upstream
// response into one reading per window. A window whose utilization is missing or
// malformed, or whose status or reset is present but malformed, yields no reading, so
// the previous good reading of that window is kept. source names where the reading
// came from: response or 429.
func quotaReadings(header http.Header, source string) []session.Reading {
	var readings []session.Reading
	for _, window := range []string{session.Window5h, session.Window7d} {
		prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
		utilization, ok := parseUtilization(header.Get(prefix + "Utilization"))
		if !ok {
			continue
		}
		reading := session.Reading{Window: window, Utilization: utilization, Source: source}
		if status := header.Get(prefix + "Status"); status != "" {
			if status != "allowed" && status != "allowed_warning" && status != "rejected" {
				continue
			}
			reading.Status = status
		}
		if reset := header.Get(prefix + "Reset"); reset != "" {
			seconds, err := strconv.ParseInt(reset, 10, 64)
			if err != nil || seconds <= 0 {
				continue
			}
			reading.ResetAt = time.Unix(seconds, 0).UTC()
		}
		readings = append(readings, reading)
	}
	return readings
}
