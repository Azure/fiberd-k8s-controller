package issuer

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/utils/clock"
	"k8s.io/utils/lru"
)

const (
	// maxLimiterKeys bounds the keys one keyed limit remembers, so a client
	// cannot grow the map. An evicted key starts over with a full bucket.
	maxLimiterKeys = 4096

	// podBurst lets a Pod that restarts a few times in a row fetch at once.
	podBurst = 3
)

// RateLimits bounds how often the grant routes answer. Each replica counts
// on its own. Every value must be positive.
type RateLimits struct {
	// PerIP is the requests per second one client address may send.
	PerIP float64

	// Global is the requests per second all clients together may send. It
	// bounds what the routes ask of the API server.
	Global float64

	// PodInterval is the time one Pod UID earns one more grant in.
	PodInterval time.Duration
}

// limiter holds the buckets of RateLimits. Every take returns how long the
// caller must wait, with ok false when the request is refused.
type limiter struct {
	clock  clock.PassiveClock
	perIP  *keyedBuckets
	global *rate.Limiter
	perPod *keyedBuckets
}

// newLimiter returns a limiter over o that reads the time from c.
func newLimiter(o RateLimits, c clock.PassiveClock) *limiter {
	return &limiter{
		clock:  c,
		perIP:  newKeyedBuckets(rate.Limit(o.PerIP), burstOf(o.PerIP)),
		global: rate.NewLimiter(rate.Limit(o.Global), burstOf(o.Global)),
		perPod: newKeyedBuckets(rate.Every(o.PodInterval), podBurst),
	}
}

// burstOf is one second of rate, and at least one request.
func burstOf(perSecond float64) int {
	return max(1, int(math.Ceil(perSecond)))
}

// admit takes one request from ip's bucket and then from the global one.
// The order matters. A client over its own limit must not drain the bucket
// every other client shares.
func (l *limiter) admit(ip string) (wait time.Duration, ok bool) {
	now := l.clock.Now()
	if wait, ok := l.perIP.take(ip, now); !ok {
		return wait, false
	}
	return take(l.global, now)
}

// admitPod takes one request from the bucket of the Pod with uid.
func (l *limiter) admitPod(uid string) (wait time.Duration, ok bool) {
	return l.perPod.take(uid, l.clock.Now())
}

// take spends one token of b at now. AllowN leaves b as it was when it
// refuses, so a client that keeps retrying builds no debt and its wait does
// not climb, as it would with a reservation that is never cancelled.
func take(b *rate.Limiter, now time.Time) (wait time.Duration, ok bool) {
	if b.AllowN(now, 1) {
		return 0, true
	}
	return time.Duration((1 - b.TokensAt(now)) / float64(b.Limit()) * float64(time.Second)), false
}

// keyedBuckets is one token bucket per key, up to maxLimiterKeys of them.
type keyedBuckets struct {
	limit rate.Limit
	burst int

	// mu makes get-or-create one step. The cache guards each of its calls
	// but not the pair, so two first requests of a key could otherwise make
	// two buckets, and each would admit a full burst.
	mu      sync.Mutex
	buckets *lru.Cache
}

func newKeyedBuckets(limit rate.Limit, burst int) *keyedBuckets {
	return &keyedBuckets{limit: limit, burst: burst, buckets: lru.New(maxLimiterKeys)}
}

// take spends one token of key's bucket at now.
func (k *keyedBuckets) take(key string, now time.Time) (wait time.Duration, ok bool) {
	return take(k.bucket(key), now)
}

// bucket returns key's bucket, creating it on first use.
func (k *keyedBuckets) bucket(key string) *rate.Limiter {
	k.mu.Lock()
	defer k.mu.Unlock()
	if b, ok := k.buckets.Get(key); ok {
		return b.(*rate.Limiter)
	}
	b := rate.NewLimiter(k.limit, k.burst)
	k.buckets.Add(key, b)
	return b
}
