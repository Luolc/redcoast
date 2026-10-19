package codex

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Luolc/redcoast/session"
)

// windows maps the window lengths the upstream reports, in minutes, to the store's
// windows; a window of another length gives no reading.
var windows = map[string]string{"300": session.Window5h, "10080": session.Window7d}

// quotaReadings parses the x-codex-{primary,secondary}-* headers of an upstream
// response into one reading per window: used-percent is the share of the window's quota
// used, window-minutes says which window it is and reset-at, in Unix seconds, when it
// resets. A window whose percentage or length is missing or malformed, whose reset is
// present but malformed, or whose length the store has no window for yields no
// reading, so the previous good reading of that window is kept. The upstream sends
// these headers on the response head of an HTTP stream, whatever its Content-Type.
func quotaReadings(header http.Header, source string) []session.Reading {
	var readings []session.Reading
	seen := map[string]bool{}
	for _, kind := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + kind + "-"
		window, known := windows[header.Get(prefix+"Window-Minutes")]
		used, err := strconv.ParseFloat(header.Get(prefix+"Used-Percent"), 64)
		// Stated positively so that NaN, which fails every comparison, is refused too.
		if !known || seen[window] || err != nil || !(used >= 0 && used <= math.MaxFloat64) {
			continue
		}
		reading := session.Reading{Window: window, Utilization: used / 100, Source: source}
		if reset := header.Get(prefix + "Reset-At"); reset != "" {
			seconds, err := strconv.ParseInt(reset, 10, 64)
			if err != nil || seconds <= 0 {
				continue
			}
			reading.ResetAt = time.Unix(seconds, 0).UTC()
		}
		seen[window] = true
		readings = append(readings, reading)
	}
	return readings
}
