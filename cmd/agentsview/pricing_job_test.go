package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/poller"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

// Resync may spend up to five seconds draining SQLite connections before a
// swap, and the surrounding work can take longer on loaded Windows runners.
const pricingResyncTestTimeout = 30 * time.Second

// pricingCatalogTransport answers the GenAI Prices, LiteLLM, and OpenRouter
// catalog requests a refresh makes and records their URLs.
type pricingCatalogTransport struct {
	requests chan *http.Request
	fail     func(*http.Request) bool
}

func (t pricingCatalogTransport) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	t.requests <- req
	if t.fail != nil && t.fail(req) {
		return nil, errPricingCatalogTransportFailure
	}
	body := `{"data": []}`
	if strings.HasSuffix(req.URL.Path, "/prices/new_data/v2/data.json") {
		// A minimal, valid GenAI Prices document. An empty array parses
		// as "no providers", which pricingrefresh treats as a fetch
		// error (see pricingrefresh.RefreshIfStale's degraded-catalog
		// tests); tests here care about the LiteLLM/OpenRouter path.
		body = `[{
			"id": "test-provider",
			"model_match": {"starts_with": "test-model"},
			"models": [{
				"id": "test-model",
				"match": {"equals": "test-model"},
				"prices": {"input_mtok": 1}
			}]
		}]`
	} else if req.URL.Host == "raw.githubusercontent.com" {
		body = `{
			"scheduled-model": {
				"input_cost_per_token": 0.000002,
				"litellm_provider": "test"
			}
		}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

var errPricingCatalogTransportFailure = &pricingCatalogTransportError{}

type pricingCatalogTransportError struct{}

func (*pricingCatalogTransportError) Error() string {
	return "simulated pricing catalog transport failure"
}

func withPricingCatalogTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = original
	})
}

// TestPricingRefreshJobFetchesEvenAfterRecentInternalAttempt exercises the
// preserved semantics from the old ticker loop: the Job always
// force-refreshes (pricingrefresh.RefreshCurrent), regardless of
// pricingrefresh's own internal "_litellm_last_attempt" cooldown meta-key.
// The internal/poller Scheduler's own Cooldown (see pricingRefreshJobOptions)
// is what gates how often the Job is invoked; once invoked, it always fetches.
func TestPricingRefreshJobFetchesEvenAfterRecentInternalAttempt(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	previousAttempt := time.Now().Add(-10 * time.Minute).UTC().Format(
		time.RFC3339,
	)
	require.NoError(t, database.SetPricingMeta(
		"_litellm_last_attempt", previousAttempt,
	))

	requests := make(chan *http.Request, 3)
	withPricingCatalogTransport(t, pricingCatalogTransport{requests: requests})

	job := newPricingRefreshJob(database, nil, time.Hour)
	require.NoError(t, job.Run(context.Background()))

	price, err := database.GetModelPricing("scheduled-model")
	require.NoError(t, err)
	require.NotNil(t, price)

	require.Equal(t,
		"https://raw.githubusercontent.com/pydantic/genai-prices/main/"+
			"prices/new_data/v2/data.json",
		(<-requests).URL.String(),
	)
	require.Equal(t,
		"https://raw.githubusercontent.com/BerriAI/litellm/main/"+
			"model_prices_and_context_window.json",
		(<-requests).URL.String(),
	)
	require.Equal(t,
		"https://openrouter.ai/api/v1/models",
		(<-requests).URL.String(),
	)
	currentAttempt, err := database.GetPricingMeta("_litellm_last_attempt")
	require.NoError(t, err)
	require.NotEqual(t, previousAttempt, currentAttempt)
}

func TestPricingRefreshJobWaitsForResyncSwap(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{})
	t.Cleanup(engine.Close)
	dbtest.EnsureTestDBAt(t, engine.ResyncTempPath())

	swapEntered := make(chan struct{})
	releaseSwap := make(chan struct{}, 1)
	swapDone := make(chan error, 1)
	go func() {
		swapDone <- engine.RunExclusive(func() error {
			close(swapEntered)
			<-releaseSwap
			if _, err := engine.SwapResyncDatabase(
				engine.ResyncTempPath(),
			); err != nil {
				return err
			}
			return engine.ResetCachesAfterSwap()
		})
	}()
	defer func() {
		select {
		case releaseSwap <- struct{}{}:
		default:
		}
	}()
	require.Eventually(t, func() bool {
		select {
		case <-swapEntered:
			return true
		default:
			return false
		}
	}, pricingResyncTestTimeout, time.Millisecond)

	requests := make(chan *http.Request, 3)
	withPricingCatalogTransport(t, pricingCatalogTransport{requests: requests})

	job := newPricingRefreshJob(database, engine, time.Hour)
	runDone := make(chan error, 1)
	go func() {
		runDone <- job.Run(context.Background())
	}()

	assert.Never(t, func() bool {
		return len(requests) > 0
	}, 50*time.Millisecond, time.Millisecond)

	releaseSwap <- struct{}{}
	var swapErr error
	require.Eventually(t, func() bool {
		select {
		case swapErr = <-swapDone:
			return true
		default:
			return false
		}
	}, pricingResyncTestTimeout, time.Millisecond)
	require.NoError(t, swapErr)

	require.Eventually(t, func() bool {
		select {
		case <-runDone:
			return true
		default:
			return false
		}
	}, pricingResyncTestTimeout, time.Millisecond)
	price, err := database.GetModelPricing("scheduled-model")
	require.NoError(t, err)
	require.NotNil(t, price)
}

func TestSeedPricingWaitsForResyncSwap(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	price, err := database.GetModelPricing("gpt-5.5")
	require.NoError(t, err)
	require.Nil(t, price)

	engine := agentsync.NewEngine(database, agentsync.EngineConfig{})
	t.Cleanup(engine.Close)
	dbtest.EnsureTestDBAt(t, engine.ResyncTempPath())

	swapEntered := make(chan struct{})
	releaseSwap := make(chan struct{}, 1)
	swapDone := make(chan error, 1)
	go func() {
		swapDone <- engine.RunExclusive(func() error {
			close(swapEntered)
			<-releaseSwap
			if _, err := engine.SwapResyncDatabase(
				engine.ResyncTempPath(),
			); err != nil {
				return err
			}
			return engine.ResetCachesAfterSwap()
		})
	}()
	defer func() {
		select {
		case releaseSwap <- struct{}{}:
		default:
		}
	}()
	require.Eventually(t, func() bool {
		select {
		case <-swapEntered:
			return true
		default:
			return false
		}
	}, pricingResyncTestTimeout, time.Millisecond)

	seedDone := make(chan struct{})
	go func() {
		seedPricing(database, engine)
		close(seedDone)
	}()
	assert.Never(t, func() bool {
		select {
		case <-seedDone:
			return true
		default:
			return false
		}
	}, 50*time.Millisecond, time.Millisecond)

	releaseSwap <- struct{}{}
	var swapErr error
	require.Eventually(t, func() bool {
		select {
		case swapErr = <-swapDone:
			return true
		default:
			return false
		}
	}, pricingResyncTestTimeout, time.Millisecond)
	require.NoError(t, swapErr)
	require.Eventually(t, func() bool {
		select {
		case <-seedDone:
			return true
		default:
			return false
		}
	}, pricingResyncTestTimeout, time.Millisecond)
	price, err = database.GetModelPricing("gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, price)
}

// TestPricingRefreshJobSchedulerRecordsAndSurvivesFailure is an integration
// test of the real pricingRefreshJob wired into a real poller.Scheduler
// through TriggerNow: a first attempt fails (network down), and the
// Scheduler must record the error without crashing, and a second
// TriggerNow (which bypasses the job's own Cooldown) must still succeed.
func TestPricingRefreshJobSchedulerRecordsAndSurvivesFailure(t *testing.T) {
	database := dbtest.OpenTestDB(t)

	requests := make(chan *http.Request, 30)
	failing := true
	withPricingCatalogTransport(t, pricingCatalogTransport{
		requests: requests,
		fail:     func(*http.Request) bool { return failing },
	})

	job := newPricingRefreshJob(database, nil, time.Hour)
	sched := poller.New()
	// RunAtStart is disabled for this test: it drives attempts explicitly
	// via TriggerNow, and a concurrent RunAtStart attempt racing the first
	// TriggerNow could consume the "failing" transport itself, recording a
	// second failure before the assertions below run and making
	// ConsecutiveFailures flaky.
	opts := pricingRefreshJobOptions()
	opts.RunAtStart = false
	sched.Register(job, opts)

	ctx := t.Context()
	sched.Start(ctx)

	err := sched.TriggerNow(pricingRefreshJobName)
	require.Error(t, err)
	statuses := sched.Status()
	require.Len(t, statuses, 1)
	assert.NotEmpty(t, statuses[0].LastError)
	assert.Equal(t, 1, statuses[0].ConsecutiveFailures)

	failing = false
	err = sched.TriggerNow(pricingRefreshJobName)
	require.NoError(t, err)
	statuses = sched.Status()
	assert.Empty(t, statuses[0].LastError)
	assert.Equal(t, 0, statuses[0].ConsecutiveFailures)
	assert.False(t, statuses[0].LastSuccess.IsZero())
}
