package daemon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

var errTrustOnce = errors.New("trust is down")

// trustOnceCounting trusts the logins in trusted, by their lower case,
// counts each lookup by login as asked, and fails those in fail.
type trustOnceCounting struct {
	trusted map[string]bool
	fail    map[string]bool
	asked   map[string]int
}

func (c *trustOnceCounting) Trusted(_ context.Context, login string) (bool, error) {
	c.asked[login]++
	if c.fail[login] {
		return false, errTrustOnce
	}
	return c.trusted[strings.ToLower(login)], nil
}

// A visit's trust asks about each login once, whatever its case, keeps
// no error, and wraps it.
func TestTrustOnceVisit(t *testing.T) {
	ctx := context.Background()
	inner := &trustOnceCounting{trusted: map[string]bool{"owner": true}, fail: map[string]bool{"mallory": true}, asked: map[string]int{}}
	v := &trustOnceVisit{trust: inner, verdicts: map[string]bool{}}
	for _, l := range []string{"owner", "Owner", "OWNER", "alice", "Alice"} {
		got, err := v.Trusted(ctx, l)
		if want := strings.EqualFold(l, "owner"); err != nil || got != want {
			t.Errorf("Trusted(%s) = %v, %v; want %v", l, got, err, want)
		}
	}
	for range 2 {
		if got, err := v.Trusted(ctx, "mallory"); got || !errors.Is(err, errTrustOnce) || !strings.Contains(err.Error(), "trust for this visit") {
			t.Errorf("Trusted(mallory) = %v, %v; want the error, wrapped", got, err)
		}
	}
	if want := map[string]int{"owner": 1, "alice": 1, "mallory": 2}; !reflect.DeepEqual(inner.asked, want) {
		t.Errorf("asked %v, want %v: each login once, and a failed one again", inner.asked, want)
	}
}

// trustOnceBegin starts a visit with no verdicts, over Step.Trust as it is
// then, trustOnceEnd drops it, and outside one the visit's Dirty calls use
// Step.Trust itself.
func TestTrustOnceBeginEnd(t *testing.T) {
	ctx := context.Background()
	st := &Step{}
	if st.trustOnceBegin(); st.trustOnce != nil || st.trustOnceTrust() != nil {
		t.Errorf("with no Trust, the visit's trust = %v, want none", st.trustOnceTrust())
	}
	first := &trustOnceCounting{trusted: map[string]bool{"owner": true}, asked: map[string]int{}}
	st.Trust = first
	if got := st.trustOnceTrust(); got != first {
		t.Errorf("outside a visit, the trust = %v, want Step.Trust", got)
	}
	st.trustOnceBegin()
	for range 2 {
		if ok, err := st.trustOnceTrust().Trusted(ctx, "owner"); !ok || err != nil {
			t.Errorf("Trusted(owner) = %v, %v; want true", ok, err)
		}
	}
	st.trustOnceEnd()
	if st.trustOnce != nil || st.trustOnceTrust() != first {
		t.Errorf("after the visit, the trust = %v, want Step.Trust", st.trustOnceTrust())
	}
	second := &trustOnceCounting{trusted: map[string]bool{}, asked: map[string]int{}}
	st.Trust = second
	st.trustOnceBegin()
	if ok, err := st.trustOnceTrust().Trusted(ctx, "owner"); ok || err != nil {
		t.Errorf("Trusted(owner) on the next visit = %v, %v; want false, as Step.Trust says now", ok, err)
	}
	// A visit cut short, with no trustOnceEnd, keeps nothing either.
	st.Trust = first
	st.trustOnceBegin()
	if ok, err := st.trustOnceTrust().Trusted(ctx, "owner"); !ok || err != nil {
		t.Errorf("Trusted(owner) after a visit cut short = %v, %v; want true, as Step.Trust says now", ok, err)
	}
	if first.asked["owner"] != 2 || second.asked["owner"] != 1 {
		t.Errorf("asked %v, then %v; want owner once on each visit", first.asked, second.asked)
	}
}
