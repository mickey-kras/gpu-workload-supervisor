package modelcheck

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Hub retry defaults. A transient hub failure, such as rate limiting or a
// server error, must not discard a full inventory run, so each hub request
// gets a few bounded retries.
const (
	defaultListingTimeout = 10 * time.Minute
	defaultRetryAttempts  = 4
	defaultRetryBase      = time.Second
	defaultRetryMax       = 30 * time.Second
	// maxRetryAfter is the longest Retry-After wait that a retry honors. A
	// longer wait gives up at once, so a run does not block for long.
	maxRetryAfter = time.Minute
)

// hubOptions tunes the hub client. The zero value gives the defaults.
type hubOptions struct {
	// listingTimeout bounds one listing. An earlier deadline of ctx stops
	// it first.
	listingTimeout time.Duration
	retry          hubRetry
}

// hubRetry bounds the retries of one hub request. The zero value gives the
// defaults.
type hubRetry struct {
	// attempts is the number of requests, the first one included.
	attempts int
	// base is the backoff limit before the first retry. It doubles for each
	// retry, up to max.
	base, max time.Duration
	// wait waits for d or until ctx ends.
	wait func(ctx context.Context, d time.Duration) error
	// jitter gives a random duration in [0, d].
	jitter func(d time.Duration) time.Duration
}

func (o hubOptions) withDefaults() hubOptions {
	if o.listingTimeout <= 0 {
		o.listingTimeout = defaultListingTimeout
	}
	r := &o.retry
	if r.attempts <= 0 {
		r.attempts = defaultRetryAttempts
	}
	if r.base <= 0 {
		r.base = defaultRetryBase
	}
	if r.max <= 0 {
		r.max = defaultRetryMax
	}
	if r.wait == nil {
		r.wait = waitContext
	}
	if r.jitter == nil {
		r.jitter = fullJitter
	}
	return o
}

// delay gives the wait before retry number retry (1 for the first retry).
// A Retry-After value of the hub wins over the backoff. ok is false when
// the hub asks for a wait above maxRetryAfter: the request then gives up.
func (r hubRetry) delay(retry int, retryAfter time.Duration, hinted bool) (time.Duration, bool) {
	if hinted {
		return retryAfter, retryAfter <= maxRetryAfter
	}
	limit := r.base
	for i := 1; i < retry && limit < r.max; i++ {
		limit *= 2
	}
	return r.jitter(min(limit, r.max)), true
}

// retryAfter reads the Retry-After header of a response: delay seconds or
// an HTTP date. hinted is false when the header is missing or not valid.
func retryAfter(response *http.Response, now time.Time) (time.Duration, bool) {
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		// Larger values only need to stay above maxRetryAfter.
		return time.Duration(min(seconds, int64(maxRetryAfter/time.Second)+1)) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(at.Sub(now), 0), true
}

// retryableStatus tells whether a hub status can pass on a later request:
// rate limiting, a request timeout or a server error.
func retryableStatus(response *http.Response) bool {
	return statusReason(response) == ReasonUpstreamUnreachable
}

func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return rand.N(d + 1)
}
