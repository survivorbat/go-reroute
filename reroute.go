package reroute

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
)

// Compile-time interface checks
var _ http.RoundTripper = new(ReRouter)

// ReRouter is a http.RoundTripper that allows you to register alternative hosts for outgoing
// HTTP requests. If the original host is unreachable or a 5xx HTTP status code the ReRouter
// will move to the next registered host and perform another attempt. If an alternative host succeeds,
// the new host is considered the primary and moved to the front of the fallback list.
type ReRouter struct {
	// Logger may be provided for monitoring purposes. Defaults to io.Discard.
	Logger *slog.Logger

	// Next is the next roundtripper to be called in the chain, defaults to http.DefaultTransport
	Next http.RoundTripper

	// fallbacks remain unexported to ensure it is only ever used internally.
	fallbacks atomic.Value // []string

	// configOnce ensures the Logger and Next are only set as defaults once
	configOnce sync.Once
}

// New instantiates a new ReRouter with options
func New(next http.RoundTripper, host string, fallbacks []string, options ...Option) (*ReRouter, error) {
	reRouter := &ReRouter{
		Next:   next,
		Logger: slog.New(slog.DiscardHandler),
	}

	errs := make([]error, len(options))

	for index, opt := range options {
		err := opt(reRouter)
		if err != nil {
			errs[index] = fmt.Errorf("failed to apply option %d: %w", index, err)
		}
	}

	err := errors.Join(errs...)
	if err != nil {
		return nil, errors.Join(errs...)
	}

	err = reRouter.SetFallbacks(host, fallbacks)
	if err != nil {
		// Error is already wrapped
		return nil, err
	}

	return reRouter, nil
}

// SetFallbacks sets the fallbacks for the ReRouter. This call is concurrency safe.
func (r *ReRouter) SetFallbacks(host string, fallbacks []string) error {
	fallbackHosts := make([]string, len(fallbacks)+1)
	errs := make([]error, len(fallbacks)+1)

	for index, host := range append([]string{host}, fallbacks...) {
		fallbackHosts[index], errs[index] = normalizeHost(host)
	}

	err := errors.Join(errs...)
	if err != nil {
		return fmt.Errorf("failed to set fallbacks: %w", err)
	}

	r.fallbacks.Store(fallbackHosts)

	return nil
}

// RoundTrip uses the registered fallbacks to loop through hosts if the initial call fails.
// If an alternative hosts succeeds, it is elected as the new primary and will be tried first in subsequent
// calls. If no alternative hosts are set, the request is directly forwarded to the next transport.
//
//nolint:cyclop // Slightly higher, but not worth splitting up
func (r *ReRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	r.ensureConfig()

	logger := r.Logger.WithGroup("rerouter").With("original-host", req.Host, "method", req.Method)

	// Keep track of the first response we got to return it if all alternatives fail
	var firstRes *http.Response
	var firstErr error

	fallbacks := r.fallbacks.Load().([]string)

	for index, fallback := range fallbacks {
		reqLogger := logger.With("target-host", fallback, "index", index)

		// DefaultTransport refills the body automatically, other transports might not
		newReq, err := cloneWithBody(req)
		if err != nil {
			return nil, fmt.Errorf("failed to clone body: %w", err)
		}

		newReq.URL.Host = fallback

		if newReq.Header.Get("Host") != "" {
			newReq.Header.Set("Host", fallback)
		}

		reqLogger.Debug("Performing request")
		fallbackRes, fallbackErr := r.Next.RoundTrip(newReq)
		if fallbackErr == nil && fallbackRes.StatusCode < 500 {
			reqLogger.Debug("Request successful")

			if firstRes != nil {
				_ = firstRes.Body.Close()
			}

			// NOTICE: It is possible that the fallbacks and index have become stale. Due to the use-case of this library,
			// this is considered an acceptable drawback.
			r.markPrimary(fallbacks, index)

			return fallbackRes, fallbackErr
		}

		switch {
		case fallbackErr != nil:
			reqLogger.Warn("Error occurred", "error", fallbackErr)
		case fallbackRes != nil:
			reqLogger.Warn("Response received", "status-code", fallbackRes.StatusCode)
		default:
			// Shouldn't happen unless the next transport is broken
			reqLogger.Error("Neither response nor error returned")
		}

		// Record first error that will be returned to the user if all fallbacks fail
		if firstRes == nil && firstErr == nil {
			firstRes = fallbackRes
			firstErr = fallbackErr
			continue
		}

		if fallbackRes != nil {
			_ = fallbackRes.Body.Close()
		}
	}

	logger.Debug("Ran out of fallbacks")

	return firstRes, firstErr
}

// markPrimary marks a host as the new primary and ensures other calls will attempt to reach this host first.
//
// NOTICE: Concurrent calls could lead to lost updates. Due to the use-case of this library, this considered acceptable.
func (r *ReRouter) markPrimary(current []string, index int) {
	if index == 0 {
		// It's already the primary
		return
	}

	// Can't write to the current list otherwise a race condition occurs
	newFallbacks := append([]string{}, current...)

	// Swap the working one to the front
	newFallbacks[0], newFallbacks[index] = newFallbacks[index], newFallbacks[0]

	r.fallbacks.Store(newFallbacks)
}

// ensureConfig ensures all config values are set and enables the use of &ReRouter{} as-is
func (r *ReRouter) ensureConfig() {
	// Since this code is all concurrent, ensure the variable setting is only performed once
	r.configOnce.Do(func() {
		// Doesn't make much sense, but bare &ReRouter{} is considered a valid instantiation with the SetFallbacks method
		if r.fallbacks.Load() == nil {
			r.fallbacks.Store(make([]string, 0))
		}

		if r.Logger == nil {
			r.Logger = slog.New(slog.DiscardHandler)
		}

		if r.Next == nil {
			r.Next = http.DefaultTransport
		}
	})
}

// normalizeHost parses the given hostURL and returns only the hostname and port.
func normalizeHost(hostURL string) (string, error) {
	// If no scheme is in the URL, add a dummy one to ensure url.Parse has a host
	if !strings.Contains(hostURL, "://") {
		hostURL = "dummy://" + strings.TrimSpace(hostURL)
	}

	parsed, err := url.Parse(hostURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse %q as URL %w", hostURL, err)
	}

	return parsed.Host, nil
}

// cloneWithBody is necessary because req.Clone only makes a shallow clone of the body
func cloneWithBody(req *http.Request) (*http.Request, error) {
	cloned := req.Clone(req.Context())

	if req.GetBody == nil {
		return cloned, nil
	}

	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("failed to get body: %w", err)
	}

	cloned.Body = body

	return cloned, nil
}
