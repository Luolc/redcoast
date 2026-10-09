package claude

import (
	"net/http"

	"github.com/Luolc/redcoast/session"
)

// healthPauseReason returns the pause reasons the gateway writes itself as they are and
// "manual" for any other: an operator's pause takes free text, which stays on the
// management interface.
func healthPauseReason(reason string) string {
	switch reason {
	case EgressMismatchReason, "upstream_401", session.QuotaPauseReason:
		return reason
	}
	return "manual"
}

// health is the health endpoint's answer: ok, or degraded with one reason per problem.
// It names accounts and the gateway's own pause reasons, never sessions or credentials.
type health struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons,omitempty"`
}

// HealthHandler returns the read-only health endpoint, GET /health. The answer is 200
// in both states, so a monitor tells a degraded gateway from an unreachable one by the
// body. Degraded means an account of the current set is paused or its last egress
// check could not read an IP, or the session store cannot be read.
func (e *Egress) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		status, err := e.store.Status(r.Context(), e.accounts.Candidates())
		writeJSON(w, http.StatusOK, e.judge(status.Accounts, err))
	})
	return mux
}

// judge gives the health answer from the accounts' state as the store reported it, or
// the store's error, and the egress checks of the current set.
func (e *Egress) judge(accounts []session.AccountStatus, err error) health {
	answer := health{Status: "ok"}
	if err != nil {
		answer.Reasons = append(answer.Reasons, "session store unavailable")
	}
	for _, a := range accounts {
		if a.PauseReason != "" {
			answer.Reasons = append(answer.Reasons, a.Alias+": paused ("+healthPauseReason(a.PauseReason)+")")
		}
	}
	for _, a := range e.accounts.set.Load().sorted() {
		if e.failedCheck(a) {
			answer.Reasons = append(answer.Reasons, a.alias+": egress check could not read the exit IP")
		}
	}
	if len(answer.Reasons) > 0 {
		answer.Status = "degraded"
	}
	return answer
}
