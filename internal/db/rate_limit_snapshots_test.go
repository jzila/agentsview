package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rlSnap builds a RateLimitSnapshot fixture with the fields every test in
// this file cares about; callers override additional fields (ResetsAt,
// CreditsHas, Vendor, ...) on the returned value.
func rlSnap(sessionID, machine, limitID, windowKind, observedAt string, usedPercent float64) RateLimitSnapshot {
	return RateLimitSnapshot{
		SessionID:     sessionID,
		Machine:       machine,
		LimitID:       limitID,
		PlanType:      "pro",
		WindowKind:    windowKind,
		WindowMinutes: 10080,
		UsedPercent:   usedPercent,
		ObservedAt:    observedAt,
	}
}

// TestInsertRateLimitSnapshots covers the write path's happy path (insert,
// dedup on re-parse, a distinct observation kept separate) alongside two
// invariants that ride along on every write: a malformed entry (missing
// limit_id/window_kind/observed_at) is skipped rather than failing the
// whole batch and the session write it is part of, and a nullable
// resets_at is preserved as nil rather than flattened to 0.
func TestInsertRateLimitSnapshots(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))

	resetsAt := int64(1789435448)
	snap := rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 95.0)
	snap.ResetsAt = &resetsAt
	snap.CreditsHas = true
	snap.CreditsBalance = "2714.1675630000"

	missingLimitID := RateLimitSnapshot{
		SessionID: "codex:sess-1", Machine: "laptop",
		WindowKind: "primary", ObservedAt: "2026-09-09T11:00:00Z",
	}
	missingWindowKind := RateLimitSnapshot{
		SessionID: "codex:sess-1", Machine: "laptop", LimitID: "codex",
		ObservedAt: "2026-09-09T12:00:00Z",
	}
	missingObservedAt := RateLimitSnapshot{
		SessionID: "codex:sess-1", Machine: "laptop", LimitID: "codex",
		WindowKind: "secondary",
	}
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
		snap, missingLimitID, missingWindowKind, missingObservedAt,
	}), "a malformed entry must not fail the whole insert")

	rows, err := d.RateLimitSnapshotHistory(
		context.Background(), RateLimitHistoryFilter{Machine: "laptop"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1, "only the well-formed entry is kept")
	row := rows[0]
	assert.Equal(t, "codex", row.Vendor, "an unset Vendor defaults to codex on write")
	assert.Equal(t, "codex:sess-1", row.SessionID)
	assert.Equal(t, "pro", row.PlanType)
	require.NotNil(t, row.ResetsAt)
	assert.EqualValues(t, resetsAt, *row.ResetsAt)
	assert.True(t, row.CreditsHas)
	assert.Equal(t, "2714.1675630000", row.CreditsBalance)
	assert.NotEmpty(t, row.DedupKey)

	// Re-inserting the identical snapshot (as a re-parse of the same
	// rollout would) must be a no-op: INSERT OR IGNORE against the
	// unique dedup_key index.
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{snap}))
	rows, err = d.RateLimitSnapshotHistory(
		context.Background(), RateLimitHistoryFilter{Machine: "laptop"},
	)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "re-inserting an identical snapshot must dedup")

	// A genuinely new observation (different observed_at) with no known
	// reset time is kept as its own row, and its nil ResetsAt must not
	// be flattened to 0.
	later := snap
	later.ObservedAt = "2026-09-09T11:00:00Z"
	later.ResetsAt = nil
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{later}))
	rows, err = d.RateLimitSnapshotHistory(
		context.Background(), RateLimitHistoryFilter{Machine: "laptop"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Nil(t, rows[1].ResetsAt, "a null resets_at must not be flattened to 0")
}

// TestLatestRateLimitSnapshots covers the happy path (one row per
// (machine, limit_id, window_kind) group, unfiltered returns every group)
// and the vendor/agent filter wiring: RateLimitFilter.Vendor and .Agent
// (the deprecated comma-separated alias) must actually reach the query,
// not just the standalone RateLimitAgentMatchesVendor helper they call.
func TestLatestRateLimitSnapshots(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-2", Agent: "codex"}))

	older := rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T09:00:00Z", 80)
	newer := rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 95)
	secondaryWindow := rlSnap("codex:sess-1", "laptop", "codex", "secondary", "2026-09-09T10:00:00Z", 10)
	// A distinct session id on a different machine: a source session
	// belongs to one machine, and the dedup key omits machine, so
	// reusing sess-1 here would collide with "newer" above.
	otherMachine := rlSnap("codex:sess-2", "desktop", "codex", "primary", "2026-09-09T10:00:00Z", 42)

	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
		older, newer, secondaryWindow, otherMachine,
	}))

	rows, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Machine: "laptop"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 2, "one row per (machine, limit_id, window_kind) group")
	byWindow := map[string]RateLimitSnapshot{}
	for _, r := range rows {
		byWindow[r.WindowKind] = r
	}
	assert.InDelta(t, 95.0, byWindow["primary"].UsedPercent, 0.001,
		"the primary group's latest row wins over the older one")
	assert.InDelta(t, 10.0, byWindow["secondary"].UsedPercent, 0.001)

	all, err := d.LatestRateLimitSnapshots(context.Background(), RateLimitFilter{})
	require.NoError(t, err)
	assert.Len(t, all, 3, "unfiltered returns groups for every machine")

	claudeVendor, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Vendor: "claude"},
	)
	require.NoError(t, err)
	assert.Empty(t, claudeVendor, "a different vendor filter matches nothing (rows default to codex)")

	excluded, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Agent: "claude"},
	)
	require.NoError(t, err)
	assert.Empty(t, excluded, "a non-matching agent filter must reach the query, not just RateLimitAgentMatchesVendor")
}

// TestLatestRateLimitSnapshots_ObservationResolution pins the invariants
// that make the observation_key column necessary: "latest" is resolved
// per whole observation (not independently per window_kind), and a NULL
// session_id left behind by a deleted session must never be used to merge
// two distinct observations together or split one observation's sibling
// windows apart.
func TestLatestRateLimitSnapshots_ObservationResolution(t *testing.T) {
	t.Run("a newer observation that omits a window supersedes it", func(t *testing.T) {
		d := testDB(t)
		require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
		require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
			rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T09:00:00Z", 80),
			rlSnap("codex:sess-1", "laptop", "codex", "secondary", "2026-09-09T09:00:00Z", 10),
			rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 95),
		}))

		rows, err := d.LatestRateLimitSnapshots(
			context.Background(), RateLimitFilter{Machine: "laptop"},
		)
		require.NoError(t, err)
		require.Len(t, rows, 1,
			"the stale secondary window must not survive once a newer observation omits it")
		assert.Equal(t, "primary", rows[0].WindowKind)
		assert.InDelta(t, 95.0, rows[0].UsedPercent, 0.001)
	})

	t.Run("two sessions observing at the same instant do not merge", func(t *testing.T) {
		d := testDB(t)
		require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
		require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-2", Agent: "codex"}))
		const shared = "2026-09-09T10:00:00Z"
		require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
			rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T09:00:00Z", 20),
			rlSnap("codex:sess-1", "laptop", "codex", "secondary", shared, 5),
			rlSnap("codex:sess-2", "laptop", "codex", "primary", shared, 77),
		}))

		rows, err := d.LatestRateLimitSnapshots(
			context.Background(), RateLimitFilter{Machine: "laptop"},
		)
		require.NoError(t, err)
		require.Len(t, rows, 1,
			"only one session's own window(s) win, not a mix of both sessions' same-instant rows")
		assert.Equal(t, "codex:sess-2", rows[0].SessionID)
		assert.Equal(t, "primary", rows[0].WindowKind)
	})

	t.Run("deleting both colliding sessions keeps their observations apart", func(t *testing.T) {
		d := testDB(t)
		require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
		require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-2", Agent: "codex"}))
		const shared = "2026-09-09T10:00:00Z"
		require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
			rlSnap("codex:sess-1", "laptop", "codex", "secondary", shared, 5),
			rlSnap("codex:sess-2", "laptop", "codex", "primary", shared, 77),
		}))
		require.NoError(t, d.DeleteSession("codex:sess-1"))
		require.NoError(t, d.DeleteSession("codex:sess-2"))

		rows, err := d.LatestRateLimitSnapshots(
			context.Background(), RateLimitFilter{Machine: "laptop"},
		)
		require.NoError(t, err)
		require.Len(t, rows, 1,
			"a shared NULL-session key must not re-merge two distinct deleted sessions' rows")
		assert.Empty(t, rows[0].SessionID)
	})

	t.Run("deleting one session keeps its sibling windows together", func(t *testing.T) {
		d := testDB(t)
		require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
		require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-2", Agent: "codex"}))
		const observedAt = "2026-09-09T10:00:00Z"
		require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
			rlSnap("codex:sess-1", "laptop", "codex", "primary", observedAt, 60),
			rlSnap("codex:sess-1", "laptop", "codex", "secondary", observedAt, 15),
			rlSnap("codex:sess-2", "desktop", "codex", "primary", observedAt, 30),
		}))
		require.NoError(t, d.DeleteSession("codex:sess-1"))

		rows, err := d.LatestRateLimitSnapshots(context.Background(), RateLimitFilter{})
		require.NoError(t, err)
		require.Len(t, rows, 3,
			"both of the deleted session's windows survive, alongside the unrelated observation")
		byKey := map[string]RateLimitSnapshot{}
		for _, r := range rows {
			byKey[r.Machine+" "+r.WindowKind] = r
		}
		assert.Empty(t, byKey["laptop primary"].SessionID)
		assert.Empty(t, byKey["laptop secondary"].SessionID)
		assert.Equal(t, "codex:sess-2", byKey["desktop primary"].SessionID,
			"the unrelated, undeleted session's row is unaffected")
	})

	t.Run("an authoritative reparse supersedes a fallback parse's rows", func(t *testing.T) {
		d := testDB(t)
		require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
		require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
			rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T09:00:00Z", 40),
		}))
		require.NoError(t, d.InsertRateLimitSnapshotsReplacingSession(
			"codex:sess-1",
			[]RateLimitSnapshot{
				rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 95),
			},
		))

		rows, err := d.LatestRateLimitSnapshots(context.Background(), RateLimitFilter{Machine: "laptop"})
		require.NoError(t, err)
		require.Len(t, rows, 1, "the superseded fallback row must not survive")
		assert.InDelta(t, 95.0, rows[0].UsedPercent, 0.001)
	})
}

// TestLatestRateLimitSnapshots_AccountIDPassesThroughAccountlessVendor: an account_id filter must not exclude a vendor whose rows carry no account.
func TestLatestRateLimitSnapshots_AccountIDPassesThroughAccountlessVendor(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "claude:sess-a", Agent: "claude"}))
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	acctA := rlSnap("claude:sess-a", "laptop", "5h", "primary", "2026-09-09T10:00:00Z", 30)
	acctA.Vendor, acctA.AccountID = "claude", "acct-a"
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{acctA, rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 50)}))
	rows, err := d.LatestRateLimitSnapshots(context.Background(), RateLimitFilter{AccountID: "acct-a"})
	require.NoError(t, err)
	assert.Len(t, rows, 2, "account A's row plus the account-less Codex row must both return")
}

// TestRateLimitFilter_AgentAloneScopesVendorInSQL: Agent alone (Vendor
// unset) must scope the SQL itself, not just an early-return check.
func TestRateLimitFilter_AgentAloneScopesVendorInSQL(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "claude:sess-a", Agent: "claude"}))
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	claude := rlSnap("claude:sess-a", "laptop", "5h", "primary", "2026-09-09T10:00:00Z", 30)
	claude.Vendor = "claude"
	codex := rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 50)
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{claude, codex}))
	latest, err := d.LatestRateLimitSnapshots(context.Background(), RateLimitFilter{Agent: "codex"})
	require.NoError(t, err)
	require.Len(t, latest, 1, "codex only, not every vendor")
	history, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{Agent: "codex"})
	require.NoError(t, err)
	require.Len(t, history, 1, "history must scope by Agent too")
}

// TestLatestRateLimitSnapshots_CombinedFiltersBindCorrectArgCount is a
// roborev-ci finding: the query interpolates its filter clause into the
// SQL text three times (bucket_ranked, latest_plan_type,
// latest_limit_name), but the args slice repeated it four times, one
// copy too many. That mismatch went unnoticed locally (this driver
// happened to tolerate the extra unused args), but roborev-ci's build
// failed on it outright for any non-empty machine, vendor, or account
// filter. Combining all three filters at once maximizes the argument
// count mismatch a regression here would need to catch.
func TestLatestRateLimitSnapshots_CombinedFiltersBindCorrectArgCount(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	snap := rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 40)
	snap.AccountID = "acct-a"
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{snap}))

	rows, err := d.LatestRateLimitSnapshots(context.Background(), RateLimitFilter{
		Machine: "laptop", Vendor: "codex", AccountID: "acct-a",
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.InDelta(t, 40.0, rows[0].UsedPercent, 0.001)
}

// TestRateLimitSnapshotHistory covers the endpoint's filters (time range,
// limit_id, window_kind, comma-separated machine IN semantics) and the
// boundary normalization a raw text comparison against observed_at would
// get wrong: a sub-second observation can otherwise sort as "less than" a
// whole-second boundary of the same instant, and a non-UTC request offset
// would not line up with the always-UTC stored value at all.
func TestRateLimitSnapshotHistory(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-2", Agent: "codex"}))

	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
		rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-07T00:00:00Z", 10),
		rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-08T00:00:00Z", 20),
		rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-09T00:00:00Z", 30),
		rlSnap("codex:sess-1", "laptop", "codex", "secondary", "2026-09-08T00:00:00Z", 5),
		rlSnap("codex:sess-1", "laptop", "premium", "primary", "2026-09-08T00:00:00Z", 15),
		rlSnap("codex:sess-2", "desktop", "codex", "primary", "2026-09-08T00:00:00Z", 50),
	}))

	t.Run("bounded by since/until, one limit/window", func(t *testing.T) {
		rows, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{
			Machine: "laptop", LimitID: "codex", WindowKind: "primary",
			Since: "2026-09-08T00:00:00Z", Until: "2026-09-08T23:59:59Z",
		})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.InDelta(t, 20.0, rows[0].UsedPercent, 0.001)
	})

	t.Run("unbounded, one limit/window, ordered ascending", func(t *testing.T) {
		rows, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{
			Machine: "laptop", LimitID: "codex", WindowKind: "primary",
		})
		require.NoError(t, err)
		require.Len(t, rows, 3)
		assert.InDelta(t, 10.0, rows[0].UsedPercent, 0.001)
		assert.InDelta(t, 30.0, rows[2].UsedPercent, 0.001)
	})

	t.Run("no limit/window filter returns every row for the machine", func(t *testing.T) {
		rows, err := d.RateLimitSnapshotHistory(
			context.Background(), RateLimitHistoryFilter{Machine: "laptop"},
		)
		require.NoError(t, err)
		assert.Len(t, rows, 5)
	})

	t.Run("comma-separated machine filter uses IN semantics", func(t *testing.T) {
		rows, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{
			Machine: "laptop,desktop", LimitID: "codex", WindowKind: "primary",
		})
		require.NoError(t, err)
		assert.Len(t, rows, 4, "laptop's 3 primary rows plus desktop's 1")
	})

	t.Run("since/until normalize sub-second and offset boundaries", func(t *testing.T) {
		// The sub-second row lands a minute after the pre-existing
		// 2026-09-08T00:00:00Z row (used_percent 20, inserted above) so
		// it gets its own dedup bucket: RateLimitSnapshotDedupKey buckets
		// observed_at to the minute, and a timestamp merely a fraction of
		// a second later would collapse into that existing row instead
		// of exercising a genuinely new one.
		require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
			rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-08T00:01:00.500000000Z", 21),
			rlSnap("codex:sess-1", "laptop", "codex", "primary", "2026-09-08T23:59:59.999999999Z", 22),
		}))
		rows, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{
			Machine: "laptop", LimitID: "codex", WindowKind: "primary",
			Since: "2026-09-08T00:00:00Z", Until: "2026-09-09T00:00:00Z",
		})
		require.NoError(t, err)
		assert.Len(t, rows, 3,
			"the boundary row, the sub-second row, and the pre-boundary row are included; the exact upper bound is not")

		offset, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{
			Machine: "laptop", LimitID: "codex", WindowKind: "primary",
			// "2026-09-07T19:00:00-05:00" is 2026-09-08T00:00:00Z.
			Since: "2026-09-07T19:00:00-05:00", Until: "2026-09-09T00:00:00Z",
		})
		require.NoError(t, err)
		assert.Len(t, offset, 3, "a non-UTC request offset must normalize to the same UTC instant")
	})
}

// TestRateLimitSnapshotHistory_DownsamplesWideRange covers the response
// size bound: a range with more matching rows than MaxPoints must come
// back downsampled to at most MaxPoints, still ordered ascending, rather
// than growing the response (and the chart's point count) with the query
// range.
func TestRateLimitSnapshotHistory_DownsamplesWideRange(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))

	const totalRows = 50
	const maxPoints = 10
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snapshots := make([]RateLimitSnapshot, 0, totalRows)
	for i := range totalRows {
		snapshots = append(snapshots, rlSnap(
			"codex:sess-1", "laptop", "codex", "primary",
			base.Add(time.Duration(i)*time.Hour).Format(time.RFC3339Nano),
			float64(i),
		))
	}
	require.NoError(t, d.InsertRateLimitSnapshots(snapshots))

	all, err := d.RateLimitSnapshotHistory(
		context.Background(), RateLimitHistoryFilter{Machine: "laptop", MaxPoints: totalRows},
	)
	require.NoError(t, err)
	require.Len(t, all, totalRows, "a limit at or above the row count returns every row")

	rows, err := d.RateLimitSnapshotHistory(
		context.Background(),
		RateLimitHistoryFilter{Machine: "laptop", MaxPoints: maxPoints},
	)
	require.NoError(t, err)
	require.LessOrEqual(t, len(rows), maxPoints,
		"a wide range must be downsampled to at most MaxPoints rows")
	assert.NotEmpty(t, rows)
	for i := 1; i < len(rows); i++ {
		prev, err := time.Parse(time.RFC3339Nano, rows[i-1].ObservedAt)
		require.NoError(t, err)
		cur, err := time.Parse(time.RFC3339Nano, rows[i].ObservedAt)
		require.NoError(t, err)
		assert.True(t, cur.After(prev), "downsampled rows must stay strictly ascending by observed_at")
	}

	unset, err := d.RateLimitSnapshotHistory(
		context.Background(), RateLimitHistoryFilter{Machine: "laptop"},
	)
	require.NoError(t, err)
	assert.Len(t, unset, totalRows,
		"a range under the default MaxPoints must be returned in full")
}

// TestRateLimitAgentMatchesVendor pins the comma-separated matching rule:
// an exact-equality check against "codex" would wrongly reject a
// selection naming codex alongside another agent (e.g. "codex,claude").
func TestRateLimitAgentMatchesVendor(t *testing.T) {
	assert.True(t, RateLimitAgentMatchesVendor("", "codex"), "no filter matches everything")
	assert.True(t, RateLimitAgentMatchesVendor("codex", "codex"))
	assert.True(t, RateLimitAgentMatchesVendor("claude,codex", "codex"), "codex named alongside another agent")
	assert.True(t, RateLimitAgentMatchesVendor("codex,claude", "codex"), "codex named first alongside another agent")
	assert.False(t, RateLimitAgentMatchesVendor("claude", "codex"), "a single other agent matches nothing")
	assert.False(t, RateLimitAgentMatchesVendor("claude,gemini", "codex"), "multiple other agents match nothing")
}

// TestCopyRateLimitSnapshotsFrom_NullsSessionIDForUnrestoredSession pins the resync-copy invariants: an unrestored source row is copied with
// session_id NULLed and its dedup_key preserved, while a session the resync's own reparse already rebuilt keeps only its fresh row.
func TestCopyRateLimitSnapshotsFrom_NullsSessionIDForUnrestoredSession(t *testing.T) {
	dir := t.TempDir()

	srcPath := filepath.Join(dir, "src.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	require.NoError(t, srcDB.UpsertSession(Session{ID: "codex:superseded", Agent: "codex"}))
	require.NoError(t, srcDB.UpsertSession(Session{ID: "codex:rebuilt", Agent: "codex"}))
	superseded := rlSnap("codex:superseded", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 0)
	require.NoError(t, srcDB.InsertRateLimitSnapshots([]RateLimitSnapshot{
		superseded, rlSnap("codex:rebuilt", "laptop", "codex", "primary", "2026-09-09T09:00:00Z", 40),
	}))
	srcDB.Close()
	wantDedupKey := RateLimitSnapshotDedupKey(superseded)

	// The destination does not restore "codex:superseded" but already reparsed "codex:rebuilt" with its own current row before the copy runs.
	dstPath := filepath.Join(dir, "dst.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()
	require.NoError(t, dstDB.UpsertSession(Session{ID: "codex:rebuilt", Agent: "codex"}))
	require.NoError(t, dstDB.InsertRateLimitSnapshots([]RateLimitSnapshot{rlSnap("codex:rebuilt", "laptop", "codex", "primary", "2026-09-09T11:00:00Z", 95)}))
	require.NoError(t, dstDB.CopyRateLimitSnapshotsFrom(srcPath, nil),
		"CopyRateLimitSnapshotsFrom must not fail when a snapshot's session was not restored")

	copied, err := dstDB.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{})
	require.NoError(t, err)
	require.Len(t, copied, 2, "unrestored row copied, rebuilt session's stale row skipped")
	for _, r := range copied {
		if r.SessionID == "codex:rebuilt" {
			assert.Equal(t, 95.0, r.UsedPercent, "the rebuilt session's stale row must not resurface")
			continue
		}
		assert.Empty(t, r.SessionID, "session_id must be NULLed for a session absent from the destination")
		assert.Equal(t, wantDedupKey, r.DedupKey, "dedup_key must be preserved from the source, not recomputed")
	}
}

// TestInsertRateLimitSnapshots_ReattachesDetachedRow: a session copied as
// NULL-session by a resync must be reattached, not ignored, once that
// session's dedup_key reappears via a normal insert.
func TestInsertRateLimitSnapshots_ReattachesDetachedRow(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	require.NoError(t, srcDB.UpsertSession(Session{ID: "codex:missing", Agent: "codex"}))
	snap := rlSnap("codex:missing", "laptop", "codex", "primary", "2026-09-09T10:00:00Z", 40)
	require.NoError(t, srcDB.InsertRateLimitSnapshots([]RateLimitSnapshot{snap}))
	srcDB.Close()

	dstDB := testDB(t)
	require.NoError(t, dstDB.CopyRateLimitSnapshotsFrom(srcPath, nil))
	detached, err := dstDB.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{})
	require.NoError(t, err)
	require.Len(t, detached, 1)
	assert.Empty(t, detached[0].SessionID, "copied row starts detached")

	require.NoError(t, dstDB.UpsertSession(Session{ID: "codex:missing", Agent: "codex"}))
	require.NoError(t, dstDB.InsertRateLimitSnapshots([]RateLimitSnapshot{snap}))
	reattached, err := dstDB.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{})
	require.NoError(t, err)
	require.Len(t, reattached, 1, "reattachment must not duplicate the row")
	assert.Equal(t, "codex:missing", reattached[0].SessionID, "the detached row must be reattached, not left stale")
}

// TestSchemaColumnMigrations_AddsSeverityToExistingRateLimitSnapshotsTable
// covers a roborev-ci finding: rate_limit_snapshots is defined directly
// in schema.sql's CREATE TABLE IF NOT EXISTS rather than via a migration
// (this whole table is unreleased, so no archive can carry the
// pre-severity dedup_key format or legacy Claude window kinds), but
// CREATE TABLE IF NOT EXISTS is a no-op against a table an earlier build
// of this same branch already created without the severity column --
// every later snapshot insert against such an archive then fails
// outright with "no such column: severity". schemaColumnMigrations
// carries an ALTER TABLE entry for exactly this column so a writable
// open still picks it up.
func TestSchemaColumnMigrations_AddsSeverityToExistingRateLimitSnapshotsTable(t *testing.T) {
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), nil)
	execRawSQLite(t, path, "ALTER TABLE rate_limit_snapshots DROP COLUMN severity")

	d, err := Open(path)
	require.NoError(t, err)
	defer d.Close()

	snap := rlSnap("claude:sess-a", "", "", "session", "2026-09-09T10:00:00Z", 30)
	snap.Vendor, snap.AccountID, snap.Severity, snap.SessionID = "claude", "acct-a", "normal", ""
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{snap}),
		"a writable open must have migrated severity back in")

	rows, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{Vendor: "claude"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "normal", rows[0].Severity)
}

// TestRateLimitBucketIndexesMigration covers a roborev-ci finding: the
// bucket_observed and bucket_jd indexes both gained a leading
// rateLimitBucketWindowKindExpr column while keeping their existing
// names, and CREATE INDEX IF NOT EXISTS is a no-op against an index
// that name already identifies -- an archive built before this change
// keeps the old, narrower index forever, and the bucket-discovery
// queries that depend on the new column ordering fall back to a full
// bucket scan. A writable open must detect the stale shape (by column
// count, since an expression column always reports a NULL name in
// pragma_index_info, old and new alike) and rebuild both indexes.
func TestRateLimitBucketIndexesMigration(t *testing.T) {
	path := tempDBPath(t, "sessions.db")
	d, err := Open(path)
	require.NoError(t, err)
	d.Close()

	requireIndexColumnCount(t, path, "idx_rate_limit_snapshots_bucket_observed", 6)
	requireIndexColumnCount(t, path, "idx_rate_limit_snapshots_bucket_jd", 7)

	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = conn.Exec(`DROP INDEX IF EXISTS idx_rate_limit_snapshots_bucket_observed`)
	require.NoError(t, err)
	_, err = conn.Exec(`CREATE INDEX idx_rate_limit_snapshots_bucket_observed
		ON rate_limit_snapshots(vendor, machine, account_id, limit_id, observed_at)`)
	require.NoError(t, err)
	_, err = conn.Exec(`DROP INDEX IF EXISTS idx_rate_limit_snapshots_bucket_jd`)
	require.NoError(t, err)
	_, err = conn.Exec(`CREATE INDEX idx_rate_limit_snapshots_bucket_jd
		ON rate_limit_snapshots(vendor, machine, account_id, limit_id, julianday(observed_at), id)`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	requireIndexColumnCount(t, path, "idx_rate_limit_snapshots_bucket_observed", 5)
	requireIndexColumnCount(t, path, "idx_rate_limit_snapshots_bucket_jd", 6)

	d, err = Open(path)
	require.NoError(t, err, "reopen must migrate the stale indexes rather than fail")
	defer d.Close()

	requireIndexColumnCount(t, path, "idx_rate_limit_snapshots_bucket_observed", 6)
	requireIndexColumnCount(t, path, "idx_rate_limit_snapshots_bucket_jd", 7)

	snap := rlSnap("claude:sess-a", "", "", "session", "2026-09-09T10:00:00Z", 30)
	snap.Vendor, snap.AccountID, snap.SessionID = "claude", "acct-a", ""
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{snap}))
	rows, err := d.LatestRateLimitSnapshots(context.Background(), RateLimitFilter{Vendor: "claude"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func requireIndexColumnCount(t *testing.T, path, index string, want int) {
	t.Helper()
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer conn.Close()
	var got int
	require.NoError(t, conn.QueryRow(
		`SELECT COUNT(*) FROM pragma_index_info(?)`, index,
	).Scan(&got))
	assert.Equal(t, want, got, "column count for index %s", index)
}

// TestCopyRateLimitSnapshotsFrom_SourceMissingSeverityColumn covers the
// other half of the same roborev-ci finding: the copy's SELECT list for
// severity substitutes a literal ” when the source predates the
// column, and that substitution must still be valid, correctly
// comma-separated SQL alongside the destination's INSERT column list
// (a naive conditional fragment previously produced invalid SQL like
// "...scope_label, ”details..." with no separating comma, aborting the
// whole resync).
func TestCopyRateLimitSnapshotsFrom_SourceMissingSeverityColumn(t *testing.T) {
	dir := t.TempDir()

	srcPath := filepath.Join(dir, "src.db")
	srcDB := testDBAtPath(t, srcPath, "src")
	snap := rlSnap("claude:sess-a", "", "", "session", "2026-09-09T10:00:00Z", 40)
	snap.Vendor, snap.AccountID, snap.Severity, snap.SessionID = "claude", "acct-a", "normal", ""
	require.NoError(t, srcDB.InsertRateLimitSnapshots([]RateLimitSnapshot{snap}))
	srcDB.Close()
	execRawSQLite(t, srcPath, "ALTER TABLE rate_limit_snapshots DROP COLUMN severity")

	dstPath := filepath.Join(dir, "dst.db")
	dstDB := testDBAtPath(t, dstPath, "dst")
	defer dstDB.Close()
	require.NoError(t, dstDB.CopyRateLimitSnapshotsFrom(srcPath, nil),
		"the copy must produce valid SQL even when the source lacks severity")

	rows, err := dstDB.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{Vendor: "claude"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Empty(t, rows[0].Severity, "severity falls back to its default when the source column is absent")
	assert.InDelta(t, 40.0, rows[0].UsedPercent, 0.001, "the rest of the row must still copy correctly")
}

// TestInsertRateLimitSnapshots_ClaudeAccountSnapshot covers a Claude
// snapshot: no session id or machine, identity keyed by account_id
// instead, and its own vendor value keeps it out of Codex-scoped queries.
func TestInsertRateLimitSnapshots_ClaudeAccountSnapshot(t *testing.T) {
	d := testDB(t)

	snap := RateLimitSnapshot{
		Vendor:       "claude",
		AccountID:    "acct-uuid-1",
		AccountLabel: "person@example.com",
		WindowKind:   "session",
		UsedPercent:  42.5,
		ResetsAt:     new(int64(1789435448)),
		ObservedAt:   "2026-09-09T10:00:00Z",
	}
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{snap}))

	rows, err := d.RateLimitSnapshotHistory(
		context.Background(),
		RateLimitHistoryFilter{Vendor: "claude", AccountID: "acct-uuid-1"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "claude", rows[0].Vendor)
	assert.Equal(t, "acct-uuid-1", rows[0].AccountID)
	assert.Equal(t, "person@example.com", rows[0].AccountLabel)
	assert.Equal(t, "session", rows[0].WindowKind)
	assert.InDelta(t, 42.5, rows[0].UsedPercent, 0.001)
	assert.Empty(t, rows[0].SessionID, "Claude snapshots carry no source session")
	assert.Empty(t, rows[0].Machine, "Claude snapshots carry no machine")

	// A Codex-vendor query must not see the Claude row, and vice versa.
	codexRows, err := d.RateLimitSnapshotHistory(
		context.Background(), RateLimitHistoryFilter{Vendor: "codex"},
	)
	require.NoError(t, err)
	assert.Empty(t, codexRows)
}

// TestInsertRateLimitSnapshots_SkipsEntriesMissingIdentityFields covers a
// roborev finding on kata s9js: a rate_limits payload missing limit_id
// (or window_kind, or observed_at) must be skipped, not fail the whole
// batch -- and, since this is normally called as part of a larger
// session write, not fail that session's ingestion either. Before this
// fix, insertRateLimitSnapshotsTx returned an error for the very first
// malformed entry and aborted the transaction, discarding every other
// (valid) snapshot in the same batch. limit_id is required only for
// Codex rows (defaulted when Vendor is unset): Claude rows never carry
// one, so requiring it unconditionally would silently drop every Claude
// snapshot.

// TestInsertRateLimitSnapshots_DistinctCodexWindowsSharingResetAndMinuteDoNotCollide
// is the storage-level counterpart of the dedup key test above: two
// distinct Codex windows (different machines) that share a resets_at and
// an observed minute must both persist rather than one silently
// discarding the other via INSERT OR IGNORE.
func TestInsertRateLimitSnapshots_DistinctCodexWindowsSharingResetAndMinuteDoNotCollide(t *testing.T) {
	d := testDB(t)

	shared := RateLimitSnapshot{
		Vendor: "codex", LimitID: "codex", PlanType: "pro",
		WindowKind: "primary", UsedPercent: 50,
		ResetsAt: new(int64(1789435448)), ObservedAt: "2026-09-09T10:00:00Z",
	}
	laptop := shared
	laptop.Machine = "laptop"
	desktop := shared
	desktop.Machine = "desktop"

	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{laptop, desktop}))

	rows, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{Vendor: "codex"})
	require.NoError(t, err)
	require.Len(t, rows, 2, "two distinct machines must not collide into one row")

	machines := map[string]bool{}
	for _, r := range rows {
		machines[r.Machine] = true
	}
	assert.True(t, machines["laptop"])
	assert.True(t, machines["desktop"])
}

// TestInsertRateLimitSnapshots_DistinctCodexSessionsSharingEverythingDoNotCollide
// covers a roborev finding: Codex's rate limit is account-wide, so two
// different concurrent sessions on the same machine can legitimately
// report the identical machine/limit_id/plan_type/window_kind/resets_at
// within the same observed minute, and ordinal alone cannot disambiguate
// them -- it is only unique within one session's own file, so two
// different sessions' events can coincidentally share the same ordinal.
// Before this fix (which restored session_id to the dedup key), the
// second session's observation collided with the first's and was
// silently discarded instead of persisted as its own row.
func TestInsertRateLimitSnapshots_DistinctCodexSessionsSharingEverythingDoNotCollide(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-a", Agent: "codex"}))
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-b", Agent: "codex"}))

	shared := RateLimitSnapshot{
		Vendor: "codex", Machine: "laptop", LimitID: "codex", PlanType: "pro",
		WindowKind: "primary", ResetsAt: new(int64(1789435448)),
		ObservedAt: "2026-09-09T10:00:00Z", Ordinal: 0,
	}
	sessA := shared
	sessA.SessionID = "codex:sess-a"
	sessA.UsedPercent = 40
	sessB := shared
	sessB.SessionID = "codex:sess-b"
	sessB.UsedPercent = 45

	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{sessA, sessB}))

	rows, err := d.RateLimitSnapshotHistory(context.Background(), RateLimitHistoryFilter{Vendor: "codex"})
	require.NoError(t, err)
	require.Len(t, rows, 2,
		"two distinct sessions' observations must not collide even sharing everything but session id")

	sessions := map[string]bool{}
	for _, r := range rows {
		sessions[r.SessionID] = true
	}
	assert.True(t, sessions["codex:sess-a"])
	assert.True(t, sessions["codex:sess-b"])
}

// TestLatestRateLimitSnapshots_DedupedSiblingStaysVisibleAlongsideNewRow
// covers a roborev finding: the dedup key excludes used_percent, and
// buckets observed_at to the minute, so a second poll within the same
// minute that changes one window's resets_at (giving it a fresh dedup
// key) while a sibling window's identity fields stay exactly the same
// (giving it the same dedup key as before) inserts only the first
// window, silently discarding the second as a duplicate. Before the
// fix, LatestRateLimitSnapshots then dropped that deduped sibling
// entirely: it requires every row in a bucket to share the exact
// observed_at of the bucket's newest row, and the deduped sibling still
// carried the earlier poll's observed_at. insertRateLimitSnapshotsTx now
// follows every duplicate hit with an UPDATE that refreshes the
// existing row's observed_at (and other mutable fields) to the incoming
// call's own values, so an unchanged window stays visible instead of
// vanishing just because nothing about it needed a new row.
func TestLatestRateLimitSnapshots_DedupedSiblingStaysVisibleAlongsideNewRow(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))

	firstReset := int64(1789435448)
	primary := RateLimitSnapshot{
		SessionID: "codex:sess-1", Machine: "laptop", LimitID: "codex", PlanType: "pro",
		WindowKind: "primary", WindowMinutes: 10080, UsedPercent: 50,
		ResetsAt: &firstReset, ObservedAt: "2026-09-09T10:00:00Z",
	}
	secondary := RateLimitSnapshot{
		SessionID: "codex:sess-1", Machine: "laptop", LimitID: "codex", PlanType: "pro",
		WindowKind: "secondary", WindowMinutes: 300, UsedPercent: 5,
		ResetsAt: &firstReset, ObservedAt: "2026-09-09T10:00:00Z",
	}
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{primary, secondary}))

	// A second poll 30 seconds later (same minute bucket): primary's
	// reset time rolled over, so it gets a new dedup key and a new row.
	// secondary is reported again with exactly the same values, so its
	// dedup key is unchanged and it would dedup away on its own.
	secondReset := int64(1789600000)
	primaryAgain := primary
	primaryAgain.ResetsAt = &secondReset
	primaryAgain.UsedPercent = 55
	primaryAgain.ObservedAt = "2026-09-09T10:00:30Z"
	secondaryAgain := secondary
	secondaryAgain.ObservedAt = "2026-09-09T10:00:30Z"
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{primaryAgain, secondaryAgain}))

	rows, err := d.LatestRateLimitSnapshots(context.Background(), RateLimitFilter{Machine: "laptop"})
	require.NoError(t, err)
	require.Len(t, rows, 2,
		"secondary must still appear even though its own row deduped away")

	byWindow := map[string]RateLimitSnapshot{}
	for _, r := range rows {
		byWindow[r.WindowKind] = r
	}
	require.Contains(t, byWindow, "primary")
	require.Contains(t, byWindow, "secondary")
	assert.InDelta(t, 55.0, byWindow["primary"].UsedPercent, 0.001)
	assert.InDelta(t, 5.0, byWindow["secondary"].UsedPercent, 0.001,
		"secondary's own value is unchanged, only its currency is refreshed")
}

// TestInsertRateLimitSnapshots_RefreshesUsedPercentWhenNoRowInBatchIsNew
// covers a roborev finding: the earlier fix only refreshed a deduped row
// when some other row in the same call was genuinely new, so a poll
// where every window's identity fields are unchanged from the previous
// poll within the same minute -- the common steady-state case, since
// resets_at rarely changes -- got no refresh at all, even though
// used_percent (excluded from the dedup key) climbed. Every duplicate
// hit must now be followed by a refresh regardless of whether any
// sibling row was freshly inserted.
func TestInsertRateLimitSnapshots_RefreshesUsedPercentWhenNoRowInBatchIsNew(t *testing.T) {
	d := testDB(t)

	reset := int64(1789435448)
	first := RateLimitSnapshot{
		Vendor: "claude", AccountID: "acct-1", AccountLabel: "Acme",
		WindowKind: "session", UsedPercent: 90,
		ResetsAt: &reset, ObservedAt: "2026-09-09T10:00:00Z",
	}
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{first}))

	// A second poll 20 seconds later (same minute bucket): every
	// identity field is unchanged, only used_percent climbed. No row in
	// this call is genuinely new.
	second := first
	second.UsedPercent = 100
	second.ObservedAt = "2026-09-09T10:00:20Z"
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{second}))

	rows, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Vendor: "claude", AccountID: "acct-1"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.InDelta(t, 100.0, rows[0].UsedPercent, 0.001,
		"used_percent must refresh even when nothing in the batch inserted a new row")
	assert.Equal(t, "2026-09-09T10:00:20Z", rows[0].ObservedAt)

	// A stale replay (an older observed_at than what is already stored)
	// must not regress the refreshed row back to an earlier value.
	stale := first
	stale.UsedPercent = 10
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{stale}))

	rows, err = d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Vendor: "claude", AccountID: "acct-1"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.InDelta(t, 100.0, rows[0].UsedPercent, 0.001,
		"an older replay must not regress an already-newer stored value")
}

// TestLatestRateLimitSnapshots_AgentSelectionRestrictsPerVendor covers a
// roborev finding: a boolean gate that checked only whether "codex"
// appeared in the shared agent selection, applied as an all-or-nothing
// switch on the whole query, both hid every vendor's cards when Claude
// alone was selected and let Claude's rows through anyway when Codex
// alone was selected (the gate passed, and the query then ran with no
// vendor restriction at all). Each vendor's rows must be included or
// excluded independently based on whether that vendor appears in the
// selection.
func TestLatestRateLimitSnapshots_AgentSelectionRestrictsPerVendor(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
		{
			SessionID: "codex:sess-1", Machine: "laptop", LimitID: "codex",
			WindowKind: "primary", ObservedAt: "2026-09-09T10:00:00Z",
		},
		{
			Vendor: "claude", AccountID: "acct-1", WindowKind: "session",
			ObservedAt: "2026-09-09T10:00:00Z",
		},
	}))

	claudeOnly, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Agent: "claude"},
	)
	require.NoError(t, err)
	require.Len(t, claudeOnly, 1, "selecting only Claude must still show Claude's own cards")
	assert.Equal(t, "claude", claudeOnly[0].Vendor)

	codexOnly, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Agent: "codex"},
	)
	require.NoError(t, err)
	require.Len(t, codexOnly, 1, "selecting only Codex must hide Claude's cards")
	assert.Equal(t, "codex", codexOnly[0].Vendor)

	both, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Agent: "codex,claude"},
	)
	require.NoError(t, err)
	assert.Len(t, both, 2, "selecting both vendors must show both")

	neither, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Agent: "gemini,cursor"},
	)
	require.NoError(t, err)
	assert.Empty(t, neither, "a selection naming neither known vendor matches nothing")
}

// TestLatestRateLimitSnapshots_MachineFilterNeverHidesClaude covers a
// roborev finding: the machine filter applied unconditionally, even
// though Claude snapshots always carry an empty machine (the poller is
// not tied to one host) and the API documents machine filtering as
// Codex-only. Selecting any machine therefore hid every Claude card.

// TestLatestRateLimitSnapshots_InterleavedNullAndNonNullPlanTypeDoesNotDuplicate
// covers the roborev finding on kata k5bm: a live Codex stream can
// alternate a limit bucket's plan_type between empty/null and a real
// value across events (observed for the "codex_bengalfox" GPT-5.3-
// Codex-Spark bucket: 730 of 4738 September events had a null
// plan_type, the rest "pro"). Since plan_type is a label rather than
// identity, this must resolve to exactly one row for the bucket/window,
// labeled with the most recently observed non-empty plan_type -- never
// two rows (one per plan_type value) for what is really one window.
func TestLatestRateLimitSnapshots_InterleavedNullAndNonNullPlanTypeDoesNotDuplicate(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))

	base := RateLimitSnapshot{
		Vendor:        "codex",
		SessionID:     "codex:sess-1",
		Machine:       "laptop",
		LimitID:       "codex_bengalfox",
		LimitName:     "GPT-5.3-Codex-Spark",
		WindowKind:    "primary",
		WindowMinutes: 300,
	}

	// Interleaved observations: pro, then null, then pro, then null
	// again (the most recent), each with a distinct resets_at so none
	// collide in the dedup key.
	events := []struct {
		planType    string
		observedAt  string
		usedPercent float64
		resetsAt    int64
	}{
		{"pro", "2026-09-01T09:00:00Z", 10, 1789000001},
		{"", "2026-09-01T10:00:00Z", 20, 1789000002},
		{"pro", "2026-09-01T11:00:00Z", 30, 1789000003},
		{"", "2026-09-01T12:00:00Z", 40, 1789000004},
	}
	var snaps []RateLimitSnapshot
	for _, e := range events {
		snap := base
		snap.PlanType = e.planType
		snap.ObservedAt = e.observedAt
		snap.UsedPercent = e.usedPercent
		snap.ResetsAt = new(int64(e.resetsAt))
		snaps = append(snaps, snap)
	}
	require.NoError(t, d.InsertRateLimitSnapshots(snaps))

	rows, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Vendor: "codex", Machine: "laptop"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1, "null/pro plan_type alternation must not fragment into two rows")

	got := rows[0]
	assert.InDelta(t, 40, got.UsedPercent, 0.001, "the most recent observation's data must win")
	assert.Equal(t, "pro", got.PlanType,
		"plan_type must resolve to the most recently observed non-empty value, not the latest raw (empty) one")
	assert.Equal(t, "GPT-5.3-Codex-Spark", got.LimitName)

	history, err := d.RateLimitSnapshotHistory(
		context.Background(), RateLimitHistoryFilter{Vendor: "codex", Machine: "laptop"},
	)
	require.NoError(t, err)
	assert.Len(t, history, 4, "history keeps every observation regardless of plan_type label resolution")
}

// TestLatestRateLimitSnapshots_ResolvesLimitNameToMostRecentNonEmptyValue
// covers the companion part of the k5bm addendum: limit_name must
// resolve the same way plan_type does -- the most recently observed
// non-empty value, not whatever the latest raw row happens to carry (a
// general "codex" limit's events always have an empty limit_name, but a
// scoped bucket's events can still mix empty and set values across a
// rollout upgrade).

// TestLatestRateLimitSnapshots_MachineFilterNeverHidesClaude covers a
// roborev finding: the machine filter applied unconditionally, even
// though Claude snapshots always carry an empty machine (the poller is
// not tied to one host) and the API documents machine filtering as
// Codex-only. Selecting any machine therefore hid every Claude card.
func TestLatestRateLimitSnapshots_MachineFilterNeverHidesClaude(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{
		{
			SessionID: "codex:sess-1", Machine: "laptop", LimitID: "codex",
			WindowKind: "primary", ObservedAt: "2026-09-09T10:00:00Z",
		},
		{
			Vendor: "claude", AccountID: "acct-1", WindowKind: "session",
			ObservedAt: "2026-09-09T10:00:00Z",
		},
	}))

	rows, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Machine: "laptop"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 2,
		"a machine filter must still constrain Codex rows and never hide Claude's")
	byVendor := map[string]bool{}
	for _, r := range rows {
		byVendor[r.Vendor] = true
	}
	assert.True(t, byVendor["codex"])
	assert.True(t, byVendor["claude"])

	otherMachine, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Machine: "desktop"},
	)
	require.NoError(t, err)
	require.Len(t, otherMachine, 1,
		"a non-matching machine still excludes the Codex row")
	assert.Equal(t, "claude", otherMachine[0].Vendor)
}

// TestLatestRateLimitSnapshots_ResolvesLimitNameToMostRecentNonEmptyValue
// covers the companion part of the k5bm addendum: limit_name must
// resolve the same way plan_type does -- the most recently observed
// non-empty value, not whatever the latest raw row happens to carry (a
// general "codex" limit's events always have an empty limit_name, but a
// scoped bucket's events can still mix empty and set values across a
// rollout upgrade).
func TestLatestRateLimitSnapshots_ResolvesLimitNameToMostRecentNonEmptyValue(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))

	base := RateLimitSnapshot{
		Vendor:        "codex",
		SessionID:     "codex:sess-1",
		Machine:       "laptop",
		LimitID:       "codex_bengalfox",
		PlanType:      "pro",
		WindowKind:    "primary",
		WindowMinutes: 300,
	}

	older := base
	older.LimitName = ""
	older.ObservedAt = "2026-09-01T09:00:00Z"
	older.UsedPercent = 10
	older.ResetsAt = new(int64(1789000001))

	newer := base
	newer.LimitName = "GPT-5.3-Codex-Spark"
	newer.ObservedAt = "2026-09-01T10:00:00Z"
	newer.UsedPercent = 20
	newer.ResetsAt = new(int64(1789000002))

	newestButEmpty := base
	newestButEmpty.LimitName = ""
	newestButEmpty.ObservedAt = "2026-09-01T11:00:00Z"
	newestButEmpty.UsedPercent = 30
	newestButEmpty.ResetsAt = new(int64(1789000003))

	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{older, newer, newestButEmpty}))

	rows, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Vendor: "codex", Machine: "laptop"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.InDelta(t, 30, rows[0].UsedPercent, 0.001, "the most recent observation's data must win")
	assert.Equal(t, "GPT-5.3-Codex-Spark", rows[0].LimitName,
		"limit_name must resolve to the most recently observed non-empty value")
}

// TestLatestRateLimitSnapshots_ResolvesLabelsAcrossWindowKindsForSameLimit
// covers a roborev finding: plan_type and limit_name are properties of
// the whole (vendor, machine, account_id, limit_id) limit, not of one
// specific window_kind within it, since Codex reports one plan/limit
// name for the account-wide limit regardless of which window(s) a given
// token_count event happens to include. Resolving each label per
// window_kind independently -- rather than per limit_id as a whole --
// would let primary and secondary drift to different labels whenever an
// observation reports only one of the two windows: a primary-only
// observation supplying a new label, followed by a primary+secondary
// observation that omits it, would otherwise leave secondary showing a
// stale or blank label while primary shows the new one, even though
// both describe the same underlying limit.
func TestLatestRateLimitSnapshots_ResolvesLabelsAcrossWindowKindsForSameLimit(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(Session{ID: "codex:sess-1", Agent: "codex"}))

	base := RateLimitSnapshot{
		Vendor: "codex", SessionID: "codex:sess-1", Machine: "laptop",
		LimitID: "codex_bengalfox", WindowMinutes: 300,
	}

	// An early observation reports both windows with no label yet.
	primaryEarly := base
	primaryEarly.WindowKind = "primary"
	primaryEarly.ObservedAt = "2026-09-01T09:00:00Z"
	primaryEarly.UsedPercent = 10
	primaryEarly.ResetsAt = new(int64(1789000001))
	secondaryEarly := base
	secondaryEarly.WindowKind = "secondary"
	secondaryEarly.ObservedAt = "2026-09-01T09:00:00Z"
	secondaryEarly.UsedPercent = 5
	secondaryEarly.ResetsAt = new(int64(1789000001))
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{primaryEarly, secondaryEarly}))

	// A later observation reports only primary (the shape a session
	// transitioning to primary-only takes), carrying a new label.
	primaryLater := base
	primaryLater.WindowKind = "primary"
	primaryLater.PlanType = "pro"
	primaryLater.LimitName = "GPT-5.3-Codex-Spark"
	primaryLater.ObservedAt = "2026-09-01T10:00:00Z"
	primaryLater.UsedPercent = 20
	primaryLater.ResetsAt = new(int64(1789000002))
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{primaryLater}))

	rows, err := d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Vendor: "codex", Machine: "laptop"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 1,
		"the stale secondary window must not survive once a newer observation omits it")
	assert.Equal(t, "primary", rows[0].WindowKind)
	assert.Equal(t, "pro", rows[0].PlanType)
	assert.Equal(t, "GPT-5.3-Codex-Spark", rows[0].LimitName)

	// A still-later observation reports primary and secondary together
	// again, with secondary omitting the label. secondary must resolve
	// to the same label primary carries, not a stale or blank one, since
	// both describe one limit.
	primaryAgain := base
	primaryAgain.WindowKind = "primary"
	primaryAgain.PlanType = "pro"
	primaryAgain.LimitName = "GPT-5.3-Codex-Spark"
	primaryAgain.ObservedAt = "2026-09-01T11:00:00Z"
	primaryAgain.UsedPercent = 30
	primaryAgain.ResetsAt = new(int64(1789000003))
	secondaryAgain := base
	secondaryAgain.WindowKind = "secondary"
	// Codex's parser reads plan_type once per token_count event and
	// stamps it onto every window that event produces (see
	// codexSessionBuilder.observeRateLimits), so both windows of one
	// real observation always agree here; only limit_name omits
	// secondary since it applies only to the model-scoped primary
	// bucket in this fixture.
	secondaryAgain.PlanType = "pro"
	secondaryAgain.ObservedAt = "2026-09-01T11:00:00Z"
	secondaryAgain.UsedPercent = 8
	secondaryAgain.ResetsAt = new(int64(1789000003))
	require.NoError(t, d.InsertRateLimitSnapshots([]RateLimitSnapshot{primaryAgain, secondaryAgain}))

	rows, err = d.LatestRateLimitSnapshots(
		context.Background(), RateLimitFilter{Vendor: "codex", Machine: "laptop"},
	)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byWindow := map[string]RateLimitSnapshot{}
	for _, r := range rows {
		byWindow[r.WindowKind] = r
	}
	require.Contains(t, byWindow, "primary")
	require.Contains(t, byWindow, "secondary")
	assert.Equal(t, "pro", byWindow["secondary"].PlanType,
		"secondary must share primary's plan_type label, not a stale or blank one")
	assert.Equal(t, "GPT-5.3-Codex-Spark", byWindow["secondary"].LimitName,
		"secondary must share primary's limit_name label, not a stale or blank one")
}

// TestRateLimitSnapshotDedupKey_SessionBearingRowKeysOnSessionObservedLimitWindowOrdinal
// pins the dedup key formula for a row with a session id (Codex today):
// SessionID, ObservedAt, LimitID, WindowKind, and Ordinal participate;
// Vendor, Machine, AccountID, and PlanType do not, since session id is
// already vendor- and event-specific.
func TestRateLimitSnapshotDedupKey_SessionBearingRowKeysOnSessionObservedLimitWindowOrdinal(t *testing.T) {
	base := RateLimitSnapshot{
		Vendor: "codex", SessionID: "codex:sess-1", Machine: "laptop",
		LimitID: "codex", PlanType: "pro", AccountID: "acct-1", WindowKind: "primary",
		ResetsAt: new(int64(100)), ObservedAt: "2026-09-09T10:00:00.100Z",
	}
	tests := []struct {
		name     string
		mutate   func(s RateLimitSnapshot) RateLimitSnapshot
		wantSame bool
	}{
		{"vendor alone does not change the key", func(s RateLimitSnapshot) RateLimitSnapshot { s.Vendor = "claude"; return s }, true},
		{"account id alone does not change the key", func(s RateLimitSnapshot) RateLimitSnapshot { s.AccountID = "acct-2"; return s }, true},
		{"machine alone does not change the key", func(s RateLimitSnapshot) RateLimitSnapshot { s.Machine = "desktop"; return s }, true},
		{"plan_type alone does not change the key", func(s RateLimitSnapshot) RateLimitSnapshot { s.PlanType = "enterprise"; return s }, true},
		{"session id", func(s RateLimitSnapshot) RateLimitSnapshot { s.SessionID = "codex:sess-2"; return s }, false},
		{"observed_at", func(s RateLimitSnapshot) RateLimitSnapshot { s.ObservedAt = "2026-09-09T10:00:00.200Z"; return s }, false},
		{"limit_id", func(s RateLimitSnapshot) RateLimitSnapshot { s.LimitID = "codex_bengalfox"; return s }, false},
		{"window_kind", func(s RateLimitSnapshot) RateLimitSnapshot { s.WindowKind = "secondary"; return s }, false},
		{"ordinal", func(s RateLimitSnapshot) RateLimitSnapshot { s.Ordinal = 1; return s }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := RateLimitSnapshotDedupKey(base)
			got := RateLimitSnapshotDedupKey(tt.mutate(base))
			if tt.wantSame {
				assert.Equal(t, want, got)
			} else {
				assert.NotEqual(t, want, got)
			}
		})
	}
}

// TestRateLimitSnapshotDedupKey_SessionLessRowKeysOnWindowIdentity pins
// the dedup key formula for a session-less row (Claude, polled rather
// than parsed): Vendor, Machine, AccountID, LimitID, PlanType,
// WindowKind, ResetsAt, and ObservedAt bucketed to the minute all
// participate; Ordinal does not, since a session-less vendor has no
// per-event ordinal to assign (always 0).
func TestRateLimitSnapshotDedupKey_SessionLessRowKeysOnWindowIdentity(t *testing.T) {
	base := RateLimitSnapshot{
		Vendor: "claude", AccountID: "acct-1", WindowKind: "session",
		ResetsAt: new(int64(100)), ObservedAt: "2026-09-09T10:00:00.100Z",
	}
	tests := []struct {
		name     string
		mutate   func(s RateLimitSnapshot) RateLimitSnapshot
		wantSame bool
	}{
		{"same minute observed_at", func(s RateLimitSnapshot) RateLimitSnapshot {
			s.ObservedAt = "2026-09-09T10:00:59.900Z"
			return s
		}, true},
		{"next minute observed_at", func(s RateLimitSnapshot) RateLimitSnapshot {
			s.ObservedAt = "2026-09-09T10:01:00Z"
			return s
		}, false},
		{"vendor", func(s RateLimitSnapshot) RateLimitSnapshot { s.Vendor = "codex"; return s }, false},
		{"account id", func(s RateLimitSnapshot) RateLimitSnapshot { s.AccountID = "acct-2"; return s }, false},
		{"machine", func(s RateLimitSnapshot) RateLimitSnapshot { s.Machine = "desktop"; return s }, false},
		{"window_kind", func(s RateLimitSnapshot) RateLimitSnapshot { s.WindowKind = "weekly"; return s }, false},
		{"resets_at", func(s RateLimitSnapshot) RateLimitSnapshot { v := int64(200); s.ResetsAt = &v; return s }, false},
		{"ordinal does not change the key (no per-event ordinal for a session-less row)", func(s RateLimitSnapshot) RateLimitSnapshot {
			s.Ordinal = 1
			return s
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := RateLimitSnapshotDedupKey(base)
			got := RateLimitSnapshotDedupKey(tt.mutate(base))
			if tt.wantSame {
				assert.Equal(t, want, got)
			} else {
				assert.NotEqual(t, want, got)
			}
		})
	}
}
