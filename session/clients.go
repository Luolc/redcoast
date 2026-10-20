package session

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"time"
)

// ClientPrograms are the client programs a launcher may name in its launch metadata.
var ClientPrograms = []string{"claude-code", "pi", "codex-cli"}

// VersionNumber is the form of a client version in the launch metadata: three numbers
// and an optional short pre-release suffix, nothing else of the --version output.
const VersionNumber = `[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}(?:-[0-9A-Za-z.]{1,24})?`

var clientVersionPattern = regexp.MustCompile(`^` + VersionNumber + `$`)

// clientColumn is the client program of the session s as SQL: the launch metadata's
// client when it is one of ClientPrograms, else empty. The metadata comes from the
// client machine, so nothing else of it reaches the dashboard. Each column built on
// it checks json_valid first, in a WHEN of its own because CASE stops at the first
// match: the store checks the metadata with Go's json.Valid, which allows deeper
// nesting than SQLite's parser, and json_extract on such a row fails the whole query.
var clientColumn = `CASE WHEN NOT json_valid(s.launch_meta) THEN ''
	WHEN json_extract(s.launch_meta, '$.client') IN ('` + strings.Join(ClientPrograms, "', '") + `')
	THEN json_extract(s.launch_meta, '$.client') ELSE '' END`

// clientVersionColumn is the launch metadata's client_version of the session s when
// it is a string, else empty; clientVersion checks its form.
const clientVersionColumn = `CASE WHEN NOT json_valid(s.launch_meta) THEN ''
	WHEN json_type(s.launch_meta, '$.client_version') = 'text' THEN json_extract(s.launch_meta, '$.client_version') ELSE '' END`

// clientVersion returns version when it has the form VersionNumber and client is
// known, else empty.
func clientVersion(client, version string) string {
	if client == "" || !clientVersionPattern.MatchString(version) {
		return ""
	}
	return version
}

// ClientCount counts the sessions of one client machine and client program.
type ClientCount struct {
	Machine  string `json:"client_machine"`
	Client   string `json:"client"`   // empty when the launcher did not name a known one
	Live     int    `json:"live"`     // not ended
	Sessions int    `json:"sessions"` // created in the daily window, live or not
}

// ClientHour is one hour's requests of the sessions of one client machine and client
// program. Requests of no session are not counted.
type ClientHour struct {
	Hour     time.Time `json:"hour"`
	Machine  string    `json:"client_machine"`
	Client   string    `json:"client"` // empty when the launcher did not name a known one
	Requests int       `json:"requests"`
}

// clients reads the session counts of the sessions live or created since dayStart, by
// client machine and client program.
func (r *Reader) clients(ctx context.Context, dayStart int64) ([]ClientCount, error) {
	counts := []ClientCount{}
	err := collect(ctx, r.store.db, &counts, func(rows *sql.Rows) (ClientCount, error) {
		var v ClientCount
		return v, rows.Scan(&v.Machine, &v.Client, &v.Live, &v.Sessions)
	}, `SELECT s.client_machine AS m, `+clientColumn+` AS c, SUM(s.ended_at IS NULL), SUM(s.created_at >= ?)
		FROM sessions s WHERE s.ended_at IS NULL OR s.created_at >= ? GROUP BY m, c ORDER BY m, c`, dayStart, dayStart)
	return counts, err
}
