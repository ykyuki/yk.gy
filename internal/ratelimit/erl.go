package ratelimit

import (
	"fmt"

	"github.com/fastly/compute-sdk-go/erl"
)

// ERL is a Limiter backed by Fastly Edge Rate Limiting (rate counter +
// penalty box). Counts are per POP and approximate, which is fine for abuse
// prevention.
type ERL struct {
	Counter    *erl.RateCounter
	PenaltyBox *erl.PenaltyBox
	Policy     Policy
}

// NewERL opens the named rate counter and penalty box.
func NewERL(counter, penaltyBox string, p Policy) *ERL {
	return &ERL{Counter: erl.OpenRateCounter(counter), PenaltyBox: erl.OpenPenaltyBox(penaltyBox), Policy: p}
}

// Allow implements Limiter.
func (l *ERL) Allow(key string) (bool, error) {
	boxed, err := l.PenaltyBox.Has(key)
	if err != nil {
		return false, fmt.Errorf("%w: penaltybox: %v", ErrUnavailable, err)
	}
	if boxed {
		return false, nil
	}
	n, err := l.Counter.LookupCount(key, erl.CounterDuration60s)
	if err != nil {
		return false, fmt.Errorf("%w: lookup: %v", ErrUnavailable, err)
	}
	if n >= l.Policy.PerMinute {
		if err := l.PenaltyBox.Add(key, l.Policy.Penalty); err != nil {
			return false, fmt.Errorf("%w: penaltybox add: %v", ErrUnavailable, err)
		}
		return false, nil
	}
	if err := l.Counter.Increment(key, 1); err != nil {
		return false, fmt.Errorf("%w: increment: %v", ErrUnavailable, err)
	}
	return true, nil
}
