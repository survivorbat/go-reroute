package reroute

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

	// fallbacks remain unexported to prevent concurrent writes to the map
	fallbacks     map[string][]string
	fallbackMutex sync.RWMutex
}

// RegisterFallbacks registers fallbacks for a host. Schemes and paths are stripped from both the
// host and the callbacks so that only the hostname and port (if any) remain.
func (r *ReRouter) RegisterFallbacks(host string, fallbacks []string) error {
	r.ensureConfig()

	hosts := make([]string, len(fallbacks)+1)
	errs := make([]error, len(fallbacks)+1)

	for index, fallback := range append([]string{host}, fallbacks...) {
		hosts[index], errs[index] = normalizeHost(fallback)
	}

	err := errors.Join(errs...)
	if err != nil {
		return err
	}

	r.fallbackMutex.Lock()
	defer r.fallbackMutex.Unlock()

	r.fallbacks[hosts[0]] = hosts

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

	r.fallbackMutex.RLock()
	fallbacks, ok := r.fallbacks[req.Host]
	r.fallbackMutex.RUnlock()

	if !ok || len(fallbacks) < 2 {
		logger.Debug("No (additional) hosts defined, passing to next roundtripper")
		return r.Next.RoundTrip(req)
	}

	logger = logger.With("fallbacks", fallbacks)

	// Keep track of the first response we got to return it if all alternatives fail
	var firstRes *http.Response
	var firstErr error

	for index, fallback := range fallbacks {
		reqLogger := logger.With("target-host", fallback, "index", index)

		newReq := req.Clone(req.Context())
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

			r.markPrimary(req.Host, fallbacks, index)

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

// markPrimary marks a host as the new primary and ensures other calls will attempt to reach this host first
func (r *ReRouter) markPrimary(original string, urls []string, index int) {
	if index == 0 {
		// It's already the primary
		return
	}

	r.fallbackMutex.Lock()
	defer r.fallbackMutex.Unlock()

	newURLs := append([]string{urls[index]}, urls[:index]...)
	newURLs = append(newURLs, urls[index+1:]...)

	r.fallbacks[original] = newURLs
}

// ensureConfig ensures all config values are set and enables the use of &ReRouter{} as-is
func (r *ReRouter) ensureConfig() {
	if r.Logger == nil {
		r.Logger = slog.New(slog.DiscardHandler)
	}

	if r.Next == nil {
		r.Next = http.DefaultTransport
	}

	if r.fallbacks == nil {
		r.fallbackMutex.Lock()
		r.fallbacks = make(map[string][]string)
		r.fallbackMutex.Unlock()
	}
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
