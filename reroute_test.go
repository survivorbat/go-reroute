package reroute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// timeout ensures that requests made to non-existent hosts don't take too long to fail
const timeout = 10 * time.Second

func TestReRouter_New_ErrorsIfOptionErrors(t *testing.T) {
	t.Parallel()
	// Arrange
	dummyOption := func(*ReRouter) error {
		return assert.AnError
	}

	// Act
	reRouter, err := New(nil, "localhost", []string{}, dummyOption)

	// Assert
	require.ErrorIs(t, err, assert.AnError)
	assert.Nil(t, reRouter)
}

// NOTICE: Tests for New implicitly test SetFallbacks

func TestReRouter_New_ReturnsErrorOnInvalidURLs(t *testing.T) {
	t.Parallel()
	// Arrange
	hosts := []string{
		"localhost",
		":://",
	}

	// Act
	reRouter, err := New(nil, "localhost", hosts)

	// Assert
	var actual *url.Error
	require.ErrorAs(t, err, &actual)

	assert.Nil(t, reRouter)
}

func TestReRouter_New_SetsExpectedURLs(t *testing.T) {
	t.Parallel()
	// Arrange
	hosts := []string{
		"bar.local/foo/bar",
		"baz.local:2030",
	}

	// Act
	reRouter, err := New(nil, "http://foo.local/foo/bar", hosts)

	// Assert
	require.NoError(t, err)
	require.NotNil(t, reRouter)

	fallbacks := reRouter.fallbacks.Load().([]string)

	require.Contains(t, fallbacks, "foo.local")

	expected := []string{"foo.local", "bar.local", "baz.local:2030"}
	assert.Equal(t, expected, fallbacks)
}

func TestReRouter_Transport_RetriesHosts(t *testing.T) {
	t.Parallel()

	server200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server200.Close)

	server400 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server400.Close)

	server500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server500.Close)

	tests := map[string]struct {
		originalURL string
		hostList    []string

		expectError        bool
		expectedURL        string
		expectedStatusCode int
	}{
		"no hosts, successful request": {
			originalURL:        server200.URL,
			expectedURL:        server200.URL,
			expectedStatusCode: http.StatusOK,
		},
		"no hosts, 400 request": {
			originalURL:        server400.URL,
			expectedURL:        server400.URL,
			expectedStatusCode: http.StatusBadRequest,
		},
		"no hosts, 500 request": {
			originalURL:        server500.URL,
			expectedURL:        server500.URL,
			expectedStatusCode: http.StatusInternalServerError,
		},
		"no hosts, connection error": {
			originalURL: "http://localhost:1",
			expectedURL: "http://localhost:1",
			expectError: true,
		},

		"fallback from 500": {
			originalURL:        server500.URL,
			hostList:           []string{server200.URL},
			expectedURL:        server200.URL,
			expectedStatusCode: http.StatusOK,
		},
		"fallback from connection error": {
			originalURL:        "http://localhost:1",
			hostList:           []string{server200.URL},
			expectedURL:        server200.URL,
			expectedStatusCode: http.StatusOK,
		},
		"fallback from connection error and 500": {
			originalURL:        "http://localhost:1",
			hostList:           []string{server500.URL, server200.URL},
			expectedURL:        server200.URL,
			expectedStatusCode: http.StatusOK,
		},

		"400 is considered successful": {
			originalURL:        server400.URL,
			hostList:           []string{server200.URL},
			expectedURL:        server400.URL,
			expectedStatusCode: http.StatusBadRequest,
		},
		"first failure is returned on response": {
			originalURL:        server500.URL,
			hostList:           []string{"http://localhost:1"},
			expectedURL:        server500.URL,
			expectedStatusCode: http.StatusInternalServerError,
		},
		"first failure is returned on error": {
			originalURL: "http://localhost:1",
			hostList:    []string{server500.URL},
			expectError: true,
		},
	}

	for name, testData := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Arrange
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, testData.originalURL, http.NoBody)
			require.NoError(t, err)

			reRouter, err := New(nil, testData.originalURL, testData.hostList)
			require.NoError(t, err)

			client := &http.Client{Timeout: timeout, Transport: reRouter}

			// Act
			res, err := client.Do(req)

			// Assert
			if testData.expectError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)

			defer res.Body.Close()

			assert.Equal(t, testData.expectedStatusCode, res.StatusCode)
			assert.Equal(t, testData.expectedURL, res.Request.URL.String())
		})
	}
}

// customTransport is necessary to test if bodies can be closed, because http.DefaultTransport
// restores them automatically.
type customTransport struct {
	requestBodies []string
}

func (c *customTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	c.requestBodies = append(c.requestBodies, string(body))

	return nil, assert.AnError
}

func TestReRouter_Transport_PreservesBodyAcrossRequests(t *testing.T) {
	t.Parallel()
	// Arrange
	body := strings.NewReader("foo")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:1", body)
	require.NoError(t, err)

	next := &customTransport{}

	reRouter, err := New(next, "http://localhost:1", []string{"http://localhost:1", "http://localhost:1"})
	require.NoError(t, err)

	client := &http.Client{Timeout: timeout, Transport: reRouter}

	// Act
	res, err := client.Do(req) //nolint:bodyclose // It's meant to be nil here

	// Assert
	require.ErrorIs(t, err, assert.AnError)
	require.Nil(t, res)

	expected := []string{"foo", "foo", "foo"}
	assert.Equal(t, expected, next.requestBodies)
}

func TestReRouter_Transport_ReassignsPrimary(t *testing.T) {
	t.Parallel()
	// Arrange
	server200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server200.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:1", http.NoBody)
	require.NoError(t, err)

	reRouter, err := New(nil, "localhost:1", []string{server200.URL})
	require.NoError(t, err)

	client := &http.Client{Timeout: timeout, Transport: reRouter}

	// Act
	res, err := client.Do(req)

	// Assert
	require.NoError(t, err)

	defer res.Body.Close()

	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, server200.URL, res.Request.URL.String())

	fallbacks := reRouter.fallbacks.Load().([]string)

	require.Len(t, fallbacks, 2, "Expected 2 hosts to be set in the fallback list")
	assert.Equal(t, server200.URL, "http://"+fallbacks[0])
	assert.Equal(t, "localhost:1", fallbacks[1])
}

const concurrentRequestCount = 10000

func TestReRouter_Transport_WorksConcurrently(t *testing.T) {
	t.Parallel()
	// Arrange
	faultyServerCache := sync.Map{}
	faultyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		index, _ := strconv.Atoi(req.URL.Query().Get("index"))

		// Only return a 200 if the number is even OR we have seen this request before
		_, seenBefore := faultyServerCache.LoadOrStore(index, true)
		if seenBefore || index%2 == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}

		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer faultyServer.Close()

	reqs := make([]*http.Request, concurrentRequestCount)

	for index := range concurrentRequestCount {
		reqs[index], _ = http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s?index=%d", faultyServer.URL, index), http.NoBody)
	}

	reRouter, err := New(nil, faultyServer.URL, []string{faultyServer.URL})
	require.NoError(t, err)

	client := &http.Client{Timeout: timeout, Transport: reRouter}

	responses := make([]*http.Response, concurrentRequestCount)
	errs := make([]error, concurrentRequestCount)

	waitGroup := new(sync.WaitGroup)
	waitGroup.Add(concurrentRequestCount)

	// Act
	for index, req := range reqs {
		go func() {
			defer waitGroup.Done()

			responses[index], errs[index] = client.Do(req) //nolint:bodyclose // It is below
			if responses[index] != nil {
				responses[index].Body.Close()
			}
		}()
	}

	// Assert
	waitGroup.Wait()

	require.NoError(t, errors.Join(errs...))

	for _, res := range responses {
		assert.Equal(t, http.StatusOK, res.StatusCode)
	}
}

func TestNormalizeHost_ReturnsExpectedHost(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"example.com":                     "example.com",
		"example.com:1234":                "example.com:1234",
		"http://example.com:1234":         "example.com:1234",
		"http://example.com:1234/foo/bar": "example.com:1234",
		"http://example.com/foo/bar":      "example.com",
	}

	for input, expected := range tests {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			// Act
			actual, err := normalizeHost(input)

			// Assert
			require.NoError(t, err)
			assert.Equal(t, expected, actual)
		})
	}
}

func TestNormalizeHost_ReturnsErrorOnInvalidURL(t *testing.T) {
	t.Parallel()
	// Act
	_, err := normalizeHost(":://")

	// Assert
	var actual *url.Error
	require.ErrorAs(t, err, &actual)
}

type ctxKey struct{}

var ctxKeyA ctxKey

func TestCloneWithBody_ClonesWithBody(t *testing.T) {
	t.Parallel()
	// Arrange
	ctx := context.WithValue(t.Context(), ctxKeyA, "b")
	body := strings.NewReader("abc")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost:1", body)
	require.NoError(t, err)

	// Act
	cloned, err := cloneWithBody(req)

	// assert
	require.NoError(t, err)

	assert.Equal(t, req.Header, cloned.Header)
	assert.Equal(t, req.Method, cloned.Method)
	assert.Equal(t, req.URL, cloned.URL)

	reqBody, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	clonedBody, err := io.ReadAll(cloned.Body)
	require.NoError(t, err)

	assert.Equal(t, "abc", string(reqBody))
	assert.Equal(t, "abc", string(clonedBody))

	assert.Equal(t, "b", req.Context().Value(ctxKeyA))
	assert.Equal(t, "b", cloned.Context().Value(ctxKeyA))
}
