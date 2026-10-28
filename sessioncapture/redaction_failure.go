package sessioncapture

import "github.com/algorhythmic/skald/internal/secretredact"

// redactionFailure carries only allowlisted policy identifiers and counts.
// Error and Unwrap preserve the existing constant fail-closed error contract.
type redactionFailure struct{ counts map[string]int64 }

func (e *redactionFailure) Error() string { return secretredact.ErrSensitive.Error() }
func (e *redactionFailure) Unwrap() error { return secretredact.ErrSensitive }

func failureForRules(counts map[string]int64) error {
	safe := map[string]int64{}
	for _, id := range secretredact.IDs() {
		if counts[id] > 0 {
			safe[id] = counts[id]
		}
	}
	if len(safe) == 0 {
		return secretredact.ErrSensitive
	}
	return &redactionFailure{counts: safe}
}

// residualFailure is the sanitizer's fail-closed postcondition. A failed scan
// cannot attribute its own error from partial matches; retain the generic code.
func residualFailure(surfaces ...[]byte) error {
	counts := secretredact.Counts()
	for _, data := range surfaces {
		if secretredact.Inspect(data, counts) != nil {
			return secretredact.ErrSensitive
		}
	}
	for _, n := range counts {
		if n > 0 {
			return failureForRules(counts)
		}
	}
	return nil
}
