package db

import (
	"database/sql"
	"time"

	"github.com/mattn/go-sqlite3"
)

const sqliteUsageDriverName = "agentsview_sqlite3"

func init() {
	sql.Register(sqliteUsageDriverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			if err := conn.RegisterFunc(
				"agentsview_timestamp_unix_micro",
				sqliteTimestampUnixMicro,
				true,
			); err != nil {
				return err
			}
			if err := conn.RegisterFunc(
				"agentsview_local_timestamp", new(sqliteLocalTimeConverter).convert, true,
			); err != nil {
				return err
			}
			if err := conn.RegisterFunc(
				"agentsview_usage_output_tokens",
				sqliteUsageOutputTokens,
				true,
			); err != nil {
				return err
			}
			return conn.RegisterFunc(
				"agentsview_usage_web_search_requests",
				parseUsageWebSearchRequests,
				true,
			)
		},
	})
}

func sqliteTimestampUnixMicro(raw string) any {
	timestamp, ok := ParseStoredTimestamp(raw)
	if !ok {
		return nil
	}
	return timestamp.UTC().UnixMicro()
}

// database/sql serializes use of each connection. Retain only its last zone,
// so an analytics scan does not reload the same timezone file for every row.
type sqliteLocalTimeConverter struct {
	zone     string
	location *time.Location
}

func (c *sqliteLocalTimeConverter) convert(value any, timezone string) any {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return nil
	}
	if c.location == nil || c.zone != timezone {
		location, err := time.LoadLocation(timezone)
		if err != nil {
			return nil
		}
		c.zone, c.location = timezone, location
	}
	return parsed.In(c.location).Format("2006-01-02 15:04:05.999999999")
}

func sqliteUsageOutputTokens(tokenJSON string) int {
	_, outputTokens, _, _ := parseUsageTokenCounters(tokenJSON)
	return outputTokens
}
