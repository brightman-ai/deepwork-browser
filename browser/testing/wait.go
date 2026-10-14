package testing

import (
	"context"
	"fmt"
	"time"
)

// ObservationReader supplies immutable measured facts to the single wait kernel.
type ObservationReader func(context.Context) (*Observation, error)

func WaitForAssertion(ctx context.Context, spec WaitSpec, read ObservationReader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if read == nil && spec.Assert != "" {
		return fmt.Errorf("observation reader required")
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if spec.DurationMs > 0 {
		timer := time.NewTimer(time.Duration(spec.DurationMs) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutMs)*time.Millisecond)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	last := "not observed"
	for {
		obs, err := read(waitCtx)
		if err != nil || obs == nil {
			last = fmt.Sprintf("observation unavailable: %v", err)
		} else {
			result := (&AssertionEngine{}).Evaluate(obs, spec.Assert)
			if result.Status == StatusPass {
				return nil
			}
			last = string(result.Status) + ": " + result.Reason
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait condition %q unknown at deadline (%s): %w", spec.Assert, last, waitCtx.Err())
		case <-ticker.C:
		}
	}
}
