package issuer

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"
)

const (
	ipA = "10.0.0.1"
	ipB = "10.0.0.2"
	ipC = "10.0.0.3"
	ipD = "10.0.0.4"

	podA = "3f2a6c1e-8b9d-4e5f-a1b2-c3d4e5f6a7b8"
	podB = "9e8d7c6b-5a4f-4e3d-b2c1-a0f9e8d7c6b5"
)

// testLimits leaves one test's limit to bind by making the others wide.
var testLimits = RateLimits{PerIP: 1000, Global: 1000, PodInterval: time.Millisecond}

// newTestLimiter returns a limiter over o and the clock that drives it.
func newTestLimiter(o RateLimits) (*limiter, *clocktesting.FakePassiveClock) {
	c := clocktesting.NewFakePassiveClock(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	return newLimiter(o, c), c
}

func TestLimiterAdmit(t *testing.T) {
	type call struct {
		ip       string
		advance  time.Duration
		want     bool
		wantWait time.Duration
	}
	cases := []struct {
		name   string
		limits RateLimits
		calls  []call
	}{
		{
			name:   "one address gets its burst, then waits for a token",
			limits: RateLimits{PerIP: 2, Global: 1000, PodInterval: time.Second},
			calls: []call{
				{ip: ipA, want: true},
				{ip: ipA, want: true},
				{ip: ipA, wantWait: 500 * time.Millisecond},
				{ip: ipA, advance: 500 * time.Millisecond, want: true},
			},
		},
		{
			name:   "one address over its limit leaves another's bucket alone",
			limits: RateLimits{PerIP: 1, Global: 1000, PodInterval: time.Second},
			calls: []call{
				{ip: ipA, want: true},
				{ip: ipA, wantWait: time.Second},
				{ip: ipB, want: true},
			},
		},
		{
			name:   "all addresses together stop at the global limit",
			limits: RateLimits{PerIP: 1000, Global: 2, PodInterval: time.Second},
			calls: []call{
				{ip: ipA, want: true},
				{ip: ipB, want: true},
				{ip: ipC, wantWait: 500 * time.Millisecond},
			},
		},
		{
			name:   "a refused address spends none of the global bucket",
			limits: RateLimits{PerIP: 1, Global: 3, PodInterval: time.Second},
			calls: []call{
				{ip: ipA, want: true},
				{ip: ipA, wantWait: time.Second},
				{ip: ipA, wantWait: time.Second},
				{ip: ipA, wantWait: time.Second},
				{ip: ipB, want: true},
				{ip: ipC, want: true},
				{ip: ipD, wantWait: time.Second / 3},
			},
		},
		{
			name:   "a client that keeps retrying builds no debt",
			limits: RateLimits{PerIP: 1, Global: 1000, PodInterval: time.Second},
			calls: []call{
				{ip: ipA, want: true},
				{ip: ipA, wantWait: time.Second},
				{ip: ipA, wantWait: time.Second},
				{ip: ipA, wantWait: time.Second},
				{ip: ipA, advance: 400 * time.Millisecond, wantWait: 600 * time.Millisecond},
				{ip: ipA, advance: 600 * time.Millisecond, want: true},
			},
		},
		{
			name:   "a rate below one still admits one request",
			limits: RateLimits{PerIP: 0.5, Global: 1000, PodInterval: time.Second},
			calls: []call{
				{ip: ipA, want: true},
				{ip: ipA, wantWait: 2 * time.Second},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, c := newTestLimiter(tc.limits)
			for i, call := range tc.calls {
				c.SetTime(c.Now().Add(call.advance))
				wait, ok := l.admit(call.ip)
				if ok != call.want || !closeTo(wait, call.wantWait) {
					t.Fatalf("call %d: admit(%s) = %v, %t, want %v, %t", i, call.ip, wait, ok, call.wantWait, call.want)
				}
			}
		})
	}
}

// closeTo allows for the rounding of a wait computed in floating point.
func closeTo(got, want time.Duration) bool {
	d := got - want
	return d > -time.Microsecond && d < time.Microsecond
}

func TestLimiterAdmitPod(t *testing.T) {
	o := testLimits
	o.PodInterval = 10 * time.Second
	l, c := newTestLimiter(o)

	for i := range podBurst {
		if wait, ok := l.admitPod(podA); !ok {
			t.Fatalf("take %d of the burst was refused, wait %v", i, wait)
		}
	}
	wait, ok := l.admitPod(podA)
	if ok || !closeTo(wait, 10*time.Second) {
		t.Fatalf("admitPod past the burst = %v, %t, want 10s, false", wait, ok)
	}
	if wait, ok := l.admitPod(podB); !ok {
		t.Fatalf("another Pod was refused, wait %v", wait)
	}
	c.SetTime(c.Now().Add(10 * time.Second))
	if wait, ok := l.admitPod(podA); !ok {
		t.Fatalf("admitPod after one interval was refused, wait %v", wait)
	}
	if _, ok := l.admitPod(podA); ok {
		t.Fatal("one interval earned two grants")
	}
}

func TestKeyedBucketsBound(t *testing.T) {
	o := testLimits
	o.PerIP = 1
	l, _ := newTestLimiter(o)
	now := l.clock.Now()

	if _, ok := l.perIP.take(ipA, now); !ok {
		t.Fatal("the first take was refused")
	}
	if _, ok := l.perIP.take(ipA, now); ok {
		t.Fatal("the second take at the same instant passed")
	}
	for i := range maxLimiterKeys {
		l.perIP.take(strconv.Itoa(i), now)
	}
	if got := l.perIP.buckets.Len(); got != maxLimiterKeys {
		t.Fatalf("the buckets hold %d keys, want %d", got, maxLimiterKeys)
	}
	if _, ok := l.perIP.take(ipA, now); !ok {
		t.Fatal("an evicted key did not start over with a full bucket")
	}
}

// TestKeyedBucketsFirstTakesShareOneBucket fails with a get and an add that
// are two steps, since each racing first take would then make its own
// bucket and pass.
func TestKeyedBucketsFirstTakesShareOneBucket(t *testing.T) {
	o := testLimits
	o.PerIP = 1
	l, _ := newTestLimiter(o)
	now := l.clock.Now()

	var passed atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 64 {
		wg.Go(func() {
			<-start
			if _, ok := l.perIP.take(ipA, now); ok {
				passed.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := passed.Load(); got != 1 {
		t.Fatalf("%d racing first takes passed, want 1", got)
	}
}
