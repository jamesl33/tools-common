package cbrest

import (
	"bufio"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/couchbase/tools-common/errutil"
	"github.com/couchbase/tools-common/log"
	"github.com/couchbase/tools-common/maths"
	"github.com/couchbase/tools-common/netutil"
	"github.com/couchbase/tools-common/ptrutil"
)

// newHTTPClient returns a new HTTP client with the given client/transport.
//
// NOTE: This is used to ensure that all uses of a HTTP client use the same configuration.
func newHTTPClient(timeout time.Duration, transport http.RoundTripper) *http.Client {
	return &http.Client{Timeout: timeout, Transport: transport}
}

// newDefaultHTTPTimeouts returns the default REST HTTP client timeouts.
func newDefaultHTTPTimeouts() netutil.HTTPTimeouts {
	return netutil.HTTPTimeouts{
		Dialer:                  ptrutil.ToPtr(DefaultDialerTimeout),
		KeepAlive:               ptrutil.ToPtr(DefaultDialerKeepAlive),
		TransportIdleConn:       ptrutil.ToPtr(DefaultTransportIdleConnTimeout),
		TransportContinue:       ptrutil.ToPtr(DefaultTransportContinueTimeout),
		TransportResponseHeader: ptrutil.ToPtr(DefaultResponseHeaderTimeout),
		TransportTLSHandshake:   ptrutil.ToPtr(DefaultTLSHandshakeTimeout),
	}
}

// enhanceError returns a more informative error using information from the given request/response.
func enhanceError(err error, request *Request, resp *http.Response) error {
	if err != nil || resp == nil {
		return err
	}

	// Attempt to read the response body, this will help improve the returned error message
	defer resp.Body.Close()
	body, _ := readBody(request.Method, request.Endpoint, resp.Body, resp.ContentLength)

	return handleResponseError(request.Method, request.Endpoint, resp.StatusCode, body)
}

// cleanupResp drains the response body and ensures it's closed.
func cleanupResp(resp *http.Response) {
	if resp == nil {
		return
	}

	defer resp.Body.Close()

	_, err := io.Copy(io.Discard, resp.Body)
	if err == nil ||
		errors.Is(err, http.ErrBodyReadAfterClose) ||
		errutil.Contains(err, "http: read on closed response body") {
		return
	}

	log.Warnf("(REST) Failed to drain response body due to unexpected error: %s", err)
}

// readBody returns the entire response body returning an informative error in the case where the response body is less
// than the expected length.
func readBody(method Method, endpoint Endpoint, reader io.Reader, contentLength int64) ([]byte, error) {
	body, err := io.ReadAll(bufio.NewReader(reader))
	if err == nil {
		return body, nil
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, &UnexpectedEndOfBodyError{
			method:   method,
			endpoint: endpoint,
			expected: contentLength,
			got:      len(body),
		}
	}

	return nil, err
}

// setAuthHeaders is a utility function which sets all the request headers which are provided by the 'AuthProvider'.
func setAuthHeaders(host string, authProvider *AuthProvider, req *http.Request) {
	// Use the auth provider to populate the credentials
	req.SetBasicAuth(authProvider.provider.GetCredentials(host))

	// Set the 'User-Agent' so that we can trace how these requests are handled by the cluster
	req.Header.Set("User-Agent", authProvider.GetUserAgent())
}

// waitForRetryAfter sleeps until we can retry the request for the given response.
//
// NOTE: Truncates the value from the 'Retry-After' header to a maximum of 60s.
func waitForRetryAfter(resp *http.Response) {
	if resp.StatusCode != http.StatusServiceUnavailable {
		return
	}

	after := resp.Header.Get("Retry-After")
	if after == "" {
		return
	}

	duration := waitForRetryDuration(after)
	if duration == 0 {
		return
	}

	time.Sleep(maths.Min(duration, time.Minute))
}

// waitForRetryDuration returns the duration to wait until we've satisfied the given 'Retry-After' header.
func waitForRetryDuration(after string) time.Duration {
	seconds, err := strconv.Atoi(after)
	if seconds != 0 || err == nil {
		return time.Duration(seconds) * time.Second
	}

	date, err := time.Parse(time.RFC1123, after)
	if err == nil {
		return time.Until(date.UTC())
	}

	return 0
}

// handleRequestError is a utility function which converts a failed REST request error (hard failure as returned by the
// standard library) into a more useful/user friendly error.
func handleRequestError(req *http.Request, err error) error {
	// If we received and unknown authority error, wrap it with our informative error explaining the alternatives
	// available to the user.
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &unknownAuth) {
		return &UnknownAuthorityError{inner: err}
	}

	// String comparisons aren't ideal for error handling, but this allows us to handle future x509 error types without
	// modification.
	if strings.HasPrefix(errutil.Unwrap(err).Error(), "x509") {
		return &UnknownX509Error{inner: err}
	}

	// If we receive an EOF error, wrap it with a useful error message containing the method/endpoint
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &SocketClosedInFlightError{method: req.Method, endpoint: req.URL.Path}
	}

	return err
}

// handleResponseError is a utility function which converts a failed REST request (soft failure i.e. the request itself
// was successful) into a more useful/user friendly error.
func handleResponseError(method Method, endpoint Endpoint, statusCode int, body []byte) error {
	switch statusCode {
	case http.StatusForbidden:
		type overlay struct {
			Permissions []string `json:"permissions"`
		}

		var data overlay

		// Purposely ignored as some endpoints may not return the permissions or a body at all. In this case we just set
		// the permissions in the AuthorizationError to nil.
		_ = json.Unmarshal(body, &data)

		return &AuthorizationError{
			method:      method,
			endpoint:    endpoint,
			permissions: data.Permissions,
		}
	case http.StatusUnauthorized:
		return &AuthenticationError{method: method, endpoint: endpoint}
	case http.StatusInternalServerError:
		return &InternalServerError{method: method, endpoint: endpoint, body: body}
	case http.StatusNotFound:
		return &EndpointNotFoundError{method: method, endpoint: endpoint}
	}

	return &UnexpectedStatusCodeError{Status: statusCode, method: method, endpoint: endpoint, body: body}
}

// shouldRetry returns a boolean indicating whether the request which returned the given error should be retried.
func shouldRetry(err error) bool {
	var (
		socketClosed *SocketClosedInFlightError
		unknownAuth  *UnknownAuthorityError
	)

	return netutil.IsTemporaryError(err) || errors.As(err, &socketClosed) || errors.As(err, &unknownAuth)
}
