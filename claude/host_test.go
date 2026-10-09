package claude

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHostGuard checks that the guard passes requests naming one of its hosts, at any
// port and with or without the page's own Origin, and refuses a foreign Host (DNS
// rebinding) or a foreign Origin with 403 and none of the guarded answer.
func TestHostGuard(t *testing.T) {
	const secret = "user@example.test"
	guard := HostGuard([]string{"100.64.0.1", "fd00::7", "gateway-a"}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(secret))
	}))
	for _, arm := range []struct {
		name, host, origin string // origin "" sends none
		want               int
	}{
		{"listen_ip", "100.64.0.1:7805", "", http.StatusOK},
		{"listen_ipv6", "[fd00::7]:7805", "", http.StatusOK},
		{"configured_name", "gateway-a:7805", "", http.StatusOK},
		{"name_in_upper_case", "Gateway-A:7804", "", http.StatusOK},
		{"name_without_port", "gateway-a", "", http.StatusOK},
		{"own_origin", "gateway-a:7805", "http://gateway-a:7805", http.StatusOK},
		{"rebound_name", "rebind.example.test:7805", "", http.StatusForbidden},
		{"rebound_name_own_origin", "rebind.example.test:7805", "http://rebind.example.test:7805", http.StatusForbidden},
		{"other_ip", "100.64.0.2:7805", "", http.StatusForbidden},
		{"suffix_of_a_name", "x.gateway-a:7805", "", http.StatusForbidden},
		{"no_host", "", "", http.StatusForbidden},
		{"foreign_origin", "gateway-a:7805", "http://site.example.test", http.StatusForbidden},
		{"origin_other_port", "gateway-a:7805", "http://gateway-a:8000", http.StatusForbidden},
		{"null_origin", "gateway-a:7805", "null", http.StatusForbidden},
	} {
		t.Run(arm.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/dashboard.json", nil)
			request.Host = arm.host
			if arm.origin != "" {
				request.Header.Set("Origin", arm.origin)
			}
			recorder := httptest.NewRecorder()
			guard.ServeHTTP(recorder, request)
			body := recorder.Body.String()
			if recorder.Code != arm.want || (arm.want == http.StatusOK) != strings.Contains(body, secret) {
				t.Fatalf("status %d, body %q; want %d", recorder.Code, body, arm.want)
			}
		})
	}
}
