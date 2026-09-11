package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchUsage_DecodesAllNullableWindows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/oauth/usage", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, oauthBetaHeader, r.Header.Get("anthropic-beta"))
		assert.Equal(t, UserAgent, r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 42.5, "resets_at": 1789435448},
			"seven_day": null,
			"seven_day_opus": {"utilization": 10, "resets_at": 1789999999},
			"seven_day_sonnet": null,
			"seven_day_oauth_apps": null,
			"seven_day_overage_included": null,
			"limits": [],
			"spend_limit": null,
			"overage": null
		}`))
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	resp, err := client.FetchUsage(context.Background(), "test-token")
	require.NoError(t, err)
	require.NotNil(t, resp.FiveHour)
	assert.InDelta(t, 42.5, resp.FiveHour.Utilization, 0.001)
	assert.EqualValues(t, 1789435448, resp.FiveHour.ResetsAt)
	assert.Nil(t, resp.SevenDay)
	require.NotNil(t, resp.SevenDayOpus)
	assert.Nil(t, resp.SevenDaySonnet)
	assert.Nil(t, resp.SevenDayOAuthApps)
	assert.Nil(t, resp.SevenDayOverageIncluded)

	windows := resp.Windows()
	require.Len(t, windows, 2, "only the two non-null windows are returned")
	assert.Equal(t, "session", windows[0].Kind,
		"the fixed-bucket fallback emits the shared \"session\" identity, not \"five_hour\"")
	assert.Equal(t, "seven_day_opus", windows[1].Kind)
}

// TestFetchUsage_DecodesRFC3339ResetsAt covers the real production wire
// shape verified 2026-09-09 against the live endpoint with a real
// account: resets_at arrives as an RFC3339 string with a numeric UTC
// offset (e.g. "2026-09-10T01:10:00.010657+00:00"), not the unix-seconds
// integer the bundle's internal schema description implied. The full
// fixture also includes several undocumented, code-named window keys
// (seven_day_cowork, tangelo, nimbus_quill, ...) and a "limits" array and
// "spend"/"extra_usage" objects shaped differently than first assumed;
// decoding must tolerate all of that without erroring, since only the
// known six windows are extracted.
func TestFetchUsage_DecodesRFC3339ResetsAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 67.0, "resets_at": "2026-09-10T01:10:00.010657+00:00", "limit_dollars": null},
			"seven_day": {"utilization": 4.0, "resets_at": "2026-09-16T22:00:00.010677+00:00"},
			"seven_day_oauth_apps": null,
			"seven_day_opus": null,
			"seven_day_sonnet": null,
			"seven_day_cowork": null,
			"seven_day_omelette": null,
			"tangelo": null,
			"iguana_necktie": null,
			"omelette_promotional": null,
			"nimbus_quill": {"utilization": 0.0, "resets_at": null},
			"cinder_cove": null,
			"extra_usage": {"is_enabled": true, "monthly_limit": 110000, "used_credits": 105754.0, "utilization": 96.14, "currency": "USD"},
			"limits": [{"kind": "session", "group": "session", "percent": 67, "resets_at": "2026-09-10T01:10:00.010657+00:00", "is_active": true}],
			"spend": {"used": {"amount_minor": 105754, "currency": "USD"}, "percent": 96},
			"member_dashboard_available": false
		}`))
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	resp, err := client.FetchUsage(context.Background(), "test-token")
	require.NoError(t, err)
	require.NotNil(t, resp.FiveHour)
	assert.InDelta(t, 67.0, resp.FiveHour.Utilization, 0.001)

	wantResetsAt, err := time.Parse(time.RFC3339Nano, "2026-09-10T01:10:00.010657+00:00")
	require.NoError(t, err)
	assert.Equal(t, wantResetsAt.Unix(), resp.FiveHour.ResetsAt)

	require.NotNil(t, resp.SevenDay)
	assert.InDelta(t, 4.0, resp.SevenDay.Utilization, 0.001)
	assert.Nil(t, resp.SevenDayOpus)
	assert.Nil(t, resp.SevenDaySonnet)
	assert.Nil(t, resp.SevenDayOAuthApps)

	windows := resp.Windows()
	require.Len(t, windows, 2, "unknown/code-named window keys are ignored")
}

func TestParseResetsAt(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int64
	}{
		{"unix seconds number", "1789435448", 1789435448},
		{"null", "null", 0},
		{"absent", "", 0},
		{"empty string", `""`, 0},
		{"numeric string", `"1789435448"`, 1789435448},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResetsAt([]byte(tt.raw))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("RFC3339 string", func(t *testing.T) {
		got, err := parseResetsAt([]byte(`"2026-09-10T01:10:00.010657+00:00"`))
		require.NoError(t, err)
		want, err := time.Parse(time.RFC3339Nano, "2026-09-10T01:10:00.010657+00:00")
		require.NoError(t, err)
		assert.Equal(t, want.Unix(), got)
	})

	t.Run("unrecognized string", func(t *testing.T) {
		_, err := parseResetsAt([]byte(`"not-a-timestamp"`))
		assert.Error(t, err)
	})

	t.Run("unsupported type", func(t *testing.T) {
		_, err := parseResetsAt([]byte(`{}`))
		assert.Error(t, err)
	})
}

func TestFetchUsage_AllWindowsNull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"five_hour": null, "seven_day": null, "seven_day_opus": null,
			"seven_day_sonnet": null, "seven_day_oauth_apps": null,
			"seven_day_overage_included": null
		}`))
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	resp, err := client.FetchUsage(context.Background(), "test-token")
	require.NoError(t, err)
	assert.Empty(t, resp.Windows())
}

func TestFetchUsage_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := client.FetchUsage(context.Background(), "test-token")
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestFetchUsage_RateLimitedWithRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := client.FetchUsage(context.Background(), "test-token")
	require.Error(t, err)
	var rl *RateLimitedError
	require.ErrorAs(t, err, &rl)
	assert.Equal(t, 30*time.Second, rl.RetryAfter)
}

func TestFetchUsage_RateLimitedWithoutRetryAfterHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := client.FetchUsage(context.Background(), "test-token")
	var rl *RateLimitedError
	require.ErrorAs(t, err, &rl)
	assert.Zero(t, rl.RetryAfter)
}

func TestFetchUsage_UnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := client.FetchUsage(context.Background(), "test-token")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnauthorized)
}

// realOauthUsageLimitsArrayFixture is the real oauth/usage response shape
// verified 2026-09-10 against a live account (kata eabb), with account
// identifiers, dollar amounts, and timestamps redacted/replaced by
// representative values but the field set, nesting, and null patterns
// preserved exactly, including the undocumented code-named buckets that
// come and go (seven_day_cowork, tangelo, nimbus_quill, ...) and the
// model-scoped weekly cap ("Fable") that appears only inside `limits`.
const realOauthUsageLimitsArrayFixture = `{
	"five_hour": {"utilization": 6.0, "resets_at": "2026-09-10T01:10:00.010657+00:00", "limit_dollars": null, "used_dollars": null, "remaining_dollars": null, "locked_reason": null},
	"seven_day": {"utilization": 6.0, "resets_at": "2026-09-16T22:00:00.010677+00:00", "limit_dollars": null, "used_dollars": null, "remaining_dollars": null, "locked_reason": null},
	"seven_day_oauth_apps": null,
	"seven_day_opus": null,
	"seven_day_sonnet": null,
	"seven_day_cowork": null,
	"seven_day_omelette": null,
	"tangelo": null,
	"iguana_necktie": null,
	"omelette_promotional": null,
	"nimbus_quill": {"utilization": 0.0, "resets_at": null, "limit_dollars": null, "used_dollars": null, "remaining_dollars": null, "locked_reason": null},
	"cinder_cove": null,
	"copper_kite": null,
	"amber_ladder": null,
	"juniper_tide": null,
	"extra_usage": {
		"is_enabled": true,
		"monthly_limit": 110000,
		"used_credits": 105754.0,
		"utilization": 96.14,
		"currency": "USD",
		"decimal_places": 2,
		"disabled_reason": null,
		"user_disabled": false,
		"spend_limit_reached": false,
		"credits_ever_enabled": true,
		"daily": null,
		"weekly": null
	},
	"limits": [
		{"kind": "session", "group": "session", "percent": 6, "severity": "normal", "resets_at": "2026-09-10T01:10:00.010657+00:00", "scope": null, "is_active": true},
		{"kind": "weekly_all", "group": "weekly", "percent": 6, "severity": "normal", "resets_at": "2026-09-16T22:00:00.010677+00:00", "scope": null, "is_active": false},
		{"kind": "weekly_scoped", "group": "weekly", "percent": 5, "severity": "normal", "resets_at": "2026-09-16T22:00:00.010841+00:00", "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}, "is_active": false}
	],
	"spend": {
		"used": {"amount_minor": 105754, "currency": "USD", "exponent": 2},
		"limit": {"amount_minor": 110000, "currency": "USD", "exponent": 2},
		"percent": 96,
		"severity": "critical",
		"enabled": true,
		"disabled_reason": null,
		"cap": {"money": null, "credits": {"amount_minor": 110000, "exponent": 2}},
		"balance": null,
		"auto_reload": null,
		"disclaimer": "Usage credits cover you when you hit your plan limits.",
		"can_purchase_credits": false,
		"can_toggle": false
	},
	"member_dashboard_available": false,
	"seven_day_breakdown": null
}`

func TestFetchUsage_DecodesRealLimitsArrayShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(realOauthUsageLimitsArrayFixture))
	}))
	defer srv.Close()

	client := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	resp, err := client.FetchUsage(context.Background(), "test-token")
	require.NoError(t, err, "unknown code-named buckets must not fail decoding")

	// The fixed buckets that matter for the "primary source" decision
	// are null, matching the live account this was captured from.
	assert.Nil(t, resp.SevenDayOpus)
	assert.Nil(t, resp.SevenDaySonnet)
	assert.Nil(t, resp.SevenDayOAuthApps)

	require.True(t, resp.HasLimits(), "limits must be treated as present")
	require.Len(t, resp.Limits, 3)

	session := resp.Limits[0]
	assert.Equal(t, "session", session.Kind)
	assert.Equal(t, "session", session.Group)
	assert.InDelta(t, 6, session.Percent, 0.001)
	assert.Equal(t, "normal", session.Severity)
	assert.True(t, session.IsActive)
	assert.Nil(t, session.Scope)
	assert.Equal(t, "session", session.WindowKind(),
		"an account-wide \"session\" entry is already the shared identity")
	assert.Equal(t, 300, session.WindowMinutes())

	weeklyAll := resp.Limits[1]
	assert.Equal(t, "weekly", weeklyAll.WindowKind(),
		"an account-wide \"weekly_all\" entry must normalize to the shared \"weekly\" identity")
	assert.Equal(t, 10080, weeklyAll.WindowMinutes())
	assert.False(t, weeklyAll.IsActive)

	weeklyScoped := resp.Limits[2]
	assert.Equal(t, "weekly_scoped", weeklyScoped.Kind)
	assert.InDelta(t, 5, weeklyScoped.Percent, 0.001)
	require.NotNil(t, weeklyScoped.Scope)
	require.NotNil(t, weeklyScoped.Scope.Model)
	require.NotNil(t, weeklyScoped.Scope.Model.DisplayName)
	assert.Equal(t, "Fable", *weeklyScoped.Scope.Model.DisplayName)
	assert.Equal(t, "Fable", weeklyScoped.Scope.Label())
	assert.Equal(t, "weekly_scoped:Fable", weeklyScoped.WindowKind(),
		"a scoped limits entry must become <kind>:<scope label>")
	assert.Equal(t, 10080, weeklyScoped.WindowMinutes())

	require.NotNil(t, resp.ExtraUsage)
	assert.True(t, resp.ExtraUsage.IsEnabled)
	assert.InDelta(t, 96.14, resp.ExtraUsage.Utilization, 0.001)
	assert.False(t, resp.ExtraUsage.SpendLimitReached)

	require.NotNil(t, resp.Spend)
	require.NotNil(t, resp.Spend.Limit)
	assert.EqualValues(t, 110000, resp.Spend.Limit.AmountMinor)
	assert.Equal(t, "USD", resp.Spend.Limit.Currency)
	assert.Equal(t, 2, resp.Spend.Limit.Exponent)
}

// TestLimitEntry_WindowKind_NormalizesAccountWideKinds covers the
// roborev finding on kata eabb/qs2f, filed as kata j5md #1: an
// account-wide "session"/"weekly_all" entry from the `limits` array must
// resolve to the same window_kind ("session"/"weekly") the statusline
// sink and the fixed-bucket fallback already use for the same underlying
// window, so ingesting from more than one source never produces two
// cards for the same window. A scoped entry, and an unmapped
// account-wide kind, must keep their own raw identity.
func TestLimitEntry_WindowKind_NormalizesAccountWideKinds(t *testing.T) {
	assert.Equal(t, "session", LimitEntry{Kind: "session"}.WindowKind())
	assert.Equal(t, "weekly", LimitEntry{Kind: "weekly_all"}.WindowKind())

	displayName := "Fable"
	scoped := LimitEntry{
		Kind:  "weekly_scoped",
		Scope: &LimitScope{Model: &LimitScopeModel{DisplayName: &displayName}},
	}
	assert.Equal(t, "weekly_scoped:Fable", scoped.WindowKind(),
		"a scoped entry keeps its own scoped identity, not a canonical alias")

	assert.Equal(t, "some_future_group", LimitEntry{Kind: "some_future_group"}.WindowKind(),
		"an unmapped account-wide kind keeps its raw identity")
}

// TestLimitEntry_WindowKind_DistinguishesSameModelOnDifferentSurfaces
// covers a roborev finding: Label() drops the surface whenever a model
// is present, so two limits scoped to the same model but different
// surfaces (or the same surface but different models) would otherwise
// collapse onto one WindowKind, merging two distinct limits into one
// current card and, whenever their reset times also happen to match,
// overwriting one with the other at dedup time. WindowKind uses
// Scope.Identity() instead, which combines model and surface only when
// both are populated.
func TestLimitEntry_WindowKind_DistinguishesSameModelOnDifferentSurfaces(t *testing.T) {
	model := "claude-sonnet-4-5"
	surfaceAPI := "api"
	surfaceCode := "claude_code"

	onAPI := LimitEntry{
		Kind:  "weekly_scoped",
		Scope: &LimitScope{Model: &LimitScopeModel{ID: &model}, Surface: &surfaceAPI},
	}
	onCode := LimitEntry{
		Kind:  "weekly_scoped",
		Scope: &LimitScope{Model: &LimitScopeModel{ID: &model}, Surface: &surfaceCode},
	}
	assert.NotEqual(t, onAPI.WindowKind(), onCode.WindowKind(),
		"the same model on two different surfaces must not share a window_kind")

	otherModel := "claude-opus-4-5"
	sameSurfaceOtherModel := LimitEntry{
		Kind:  "weekly_scoped",
		Scope: &LimitScope{Model: &LimitScopeModel{ID: &otherModel}, Surface: &surfaceAPI},
	}
	assert.NotEqual(t, onAPI.WindowKind(), sameSurfaceOtherModel.WindowKind(),
		"the same surface with two different models must not share a window_kind")
}

func TestLimitScope_Label(t *testing.T) {
	displayName := "Fable"
	id := "claude-fable-1"
	surface := "api"

	tests := []struct {
		name  string
		scope *LimitScope
		want  string
	}{
		{"nil scope", nil, ""},
		{"empty scope", &LimitScope{}, ""},
		{"display name wins", &LimitScope{Model: &LimitScopeModel{DisplayName: &displayName, ID: &id}}, "Fable"},
		{"falls back to id", &LimitScope{Model: &LimitScopeModel{ID: &id}}, "claude-fable-1"},
		{"falls back to surface", &LimitScope{Surface: &surface}, "api"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.scope.Label())
		})
	}
}
