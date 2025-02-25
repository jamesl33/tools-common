package retry

import (
	"context"
	"math"
	"time"

	"github.com/couchbase/tools-common/maths"
)

// RetryableFunc represents a function which is retryable.
type RetryableFunc func(ctx *Context) (any, error)

// Retryer is a function retryer, which supports executing a given function a number of times until successful.
type Retryer struct {
	options RetryerOptions
}

// NewRetryer returns a new retryer with the given options.
func NewRetryer(options RetryerOptions) Retryer {
	// Not all options are required, but we use sane defaults otherwise behavior may be undesired/unexpected
	options.defaults()

	return Retryer{options: options}
}

// Do executes the given function until it's successful.
func (r Retryer) Do(fn RetryableFunc) (any, error) {
	return r.DoWithContext(context.Background(), fn)
}

// DoWithContext executes the given function until it's successful, the provided context may be used for cancellation.
func (r Retryer) DoWithContext(ctx context.Context, fn RetryableFunc) (any, error) {
	var (
		wrapped = NewContext(ctx)
		payload any
		done    bool
		err     error
	)

	for ; wrapped.attempt <= r.options.MaxRetries; wrapped.attempt++ {
		payload, done, err = r.do(wrapped, fn)
		if done {
			return payload, err
		}

		// Log all but the last failure, the caller may use this to log that a retry is about to take place
		if r.options.Log != nil && wrapped.attempt != r.options.MaxRetries {
			r.options.Log(wrapped, payload, err)
		}
	}

	return payload, &RetriesExhaustedError{attempts: r.options.MaxRetries, err: err}
}

// do executes the given function, returning the payload error and whether retries should stop.
func (r Retryer) do(ctx *Context, fn RetryableFunc) (any, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, true, &RetriesAbortedError{attempts: ctx.attempt - 1, err: err}
	}

	payload, err := fn(ctx)

	if !r.retry(ctx, payload, err) {
		return payload, true, err
	}

	// NOTE: Run cleanup for all but the last attempt, the caller may want to use the payload from the final attempt
	if r.options.Cleanup != nil && ctx.attempt < r.options.MaxRetries {
		r.options.Cleanup(payload)
	}

	if err := r.sleep(ctx); err != nil {
		return nil, true, err
	}

	return payload, false, err
}

// retry returns a boolean indicating whether the function should be executed again.
//
// NOTE: Users may supply a custom 'ShouldRetry' function for more complex retry behavior which depends on the payload.
func (r Retryer) retry(ctx *Context, payload any, err error) bool {
	if r.options.ShouldRetry != nil {
		return r.options.ShouldRetry(ctx, payload, err)
	}

	return err != nil
}

// sleep until the next retry attempt, or the given context is cancelled.
func (r Retryer) sleep(ctx *Context) error {
	timer := time.NewTimer(r.duration(ctx.Attempt()))
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return &RetriesAbortedError{attempts: ctx.attempt, err: ctx.Err()}
	}
}

// duration returns the duration to sleep for, this may be calculated using one of a number of different algorithms.
func (r Retryer) duration(attempt int) time.Duration {
	var n time.Duration

	switch r.options.Algorithm {
	case AlgorithmLinear:
		n = time.Duration(attempt)
	case AlgorithmExponential:
		n = 1 << attempt
	case AlgorithmFibonacci:
		n = time.Duration(math.Round(math.Pow(math.Phi, float64(attempt)) / sqrt5))
	}

	duration := n * r.options.MinDelay

	// If we overflow, just return the max delay
	if n != duration/r.options.MinDelay {
		return r.options.MaxDelay
	}

	duration = maths.Max(r.options.MinDelay, duration)
	duration = maths.Min(r.options.MaxDelay, duration)

	return duration
}
