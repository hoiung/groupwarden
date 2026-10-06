package reconcile_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/reconcile"
)

// TestSweepBacksOffOnRateLimit: when WhatsApp answers the sweep with its
// rate-limit error, the sweep waits (doubling) and tries again instead of
// skipping the group or hammering WhatsApp; other errors are not retried.
func TestSweepBacksOffOnRateLimit(t *testing.T) {
	k := modtest.New(t, "")
	limited := fmt.Errorf("%w: info query returned status 429", client.ErrRateLimited)
	k.Fake.FailNext("JoinRequests", limited, limited)
	k.Ban(modtest.Other1M)
	k.Fake.Requests = map[client.JID][]client.JoinRequest{modtest.G1: {{JID: modtest.Other1}}}
	s := &reconcile.Sweep{Enforcer: k.Enforcer, Directory: k.Dir, Config: k.Holder, Log: k.Log, Sleep: k.Clock.Sleep}
	res, err := s.Run(k.Ctx, "run1")
	if err != nil {
		t.Fatal(err)
	}
	if slept := k.Clock.Slept(); len(slept) != 2 || slept[0] != 30*time.Second || slept[1] != time.Minute {
		t.Fatalf("waits %v, want 30s then 1m", slept)
	}
	if res.RateLimited != 2 || res.Errors != 0 || res.Groups != 3 {
		t.Fatalf("result %+v", res)
	}
	if n := k.Fake.Count("JoinRequests " + string(modtest.G1)); n != 3 {
		t.Fatalf("asked %d times, want 3 (two refusals, then the answer)", n)
	}
	k.Fire()
	if k.Fake.Count("RejectJoinRequests "+string(modtest.G1)+" "+string(modtest.Other1)) != 1 {
		t.Fatalf("the request was not rejected after the back-off: %v", k.Fake.Calls())
	}
	// Any other error is logged and the sweep moves on, without waiting.
	k.Fake.FailNext("JoinRequests", errors.New("connection reset"))
	res, err = s.Run(k.Ctx, "run2")
	if err != nil || res.Errors != 1 || len(k.Clock.Slept()) != 2 {
		t.Fatalf("result %+v %v, waits %v", res, err, k.Clock.Slept())
	}
}
