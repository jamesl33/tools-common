package restutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSendJSONResponse(t *testing.T) {
	var (
		errorOccurred error
		statusCode    int
		data          []byte
	)

	errLog := func(err error) {
		errorOccurred = err
	}

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		SendJSONResponse(statusCode, data, w, errLog)
	}))
	defer testServer.Close()

	type testCase struct {
		name       string
		statusCode int
		data       []byte
	}

	cases := []testCase{
		{
			name:       "OKNilData",
			statusCode: http.StatusOK,
		},
		{
			name:       "OKEmptyData",
			statusCode: http.StatusOK,
			data:       []byte{},
		},
		{
			name:       "OKWithBody",
			statusCode: http.StatusOK,
			data:       []byte(`{"something":"something"}`),
		},
		{
			name:       "500WithBody",
			statusCode: http.StatusInternalServerError,
			data:       []byte(`{"something":"something"}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errorOccurred = nil
			statusCode = tc.statusCode
			data = tc.data

			res, err := http.Get(testServer.URL + "/")
			require.NoError(t, err)
			require.NoError(t, errorOccurred)

			defer res.Body.Close()

			require.Equal(t, tc.statusCode, res.StatusCode)
			require.Equal(t, "application/json", res.Header.Get("Content-Type"))
			require.EqualValues(t, len(tc.data), res.ContentLength)

			if len(tc.data) == 0 {
				return
			}

			body, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			require.Equal(t, tc.data, body)
		})
	}
}

func TestHandleErrorWithExtras(t *testing.T) {
	var (
		errRes        ErrorResponse
		errorOccurred error
	)

	errLog := func(err error) {
		errorOccurred = err
	}

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		HandleErrorWithExtras(errRes, w, errLog)
	}))
	defer testServer.Close()

	type testCase struct {
		name   string
		errRes ErrorResponse
	}

	cases := []testCase{
		{
			name:   "NoExtras",
			errRes: ErrorResponse{Status: http.StatusNotFound, Msg: "not found"},
		},
		{
			name:   "Extras",
			errRes: ErrorResponse{Status: http.StatusNotFound, Msg: "not found", Extras: "we did look"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errorOccurred = nil
			errRes = tc.errRes

			res, err := http.Get(testServer.URL + "/")
			require.NoError(t, err)
			require.NoError(t, errorOccurred)

			defer res.Body.Close()

			require.Equal(t, tc.errRes.Status, res.StatusCode)
			require.Equal(t, "application/json", res.Header.Get("Content-Type"))

			require.NoError(t, err)
			require.NotEqual(t, 0, res.ContentLength)

			var outRes ErrorResponse
			require.NoError(t, json.NewDecoder(res.Body).Decode(&outRes))
			require.Equal(t, tc.errRes, outRes)
		})
	}
}

func TestMarshalAndSendResponse(t *testing.T) {
	var (
		errorOccurred error
		statusCode    int
		data          any
	)

	errLog := func(err error) {
		errorOccurred = err
	}

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		MarshalAndSend(statusCode, data, w, errLog)
	}))
	defer testServer.Close()

	type testCase struct {
		name       string
		statusCode int
		data       any
		expected   []byte
	}

	cases := []testCase{
		{
			name:       "OKNilData",
			statusCode: http.StatusOK,
			expected:   []byte{},
		},
		{
			name:       "OKEmptyArray",
			statusCode: http.StatusOK,
			data:       []int{},
			expected:   []byte(`[]`),
		},
		{
			name:       "OKWithBody",
			statusCode: http.StatusOK,
			data:       []int{1, 2, 3},
			expected:   []byte(`[1,2,3]`),
		},
		{
			name:       "cannotMarshall",
			statusCode: http.StatusInternalServerError,
			data:       json.RawMessage(`{"x":0`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errorOccurred = nil
			statusCode = tc.statusCode
			data = tc.data

			res, err := http.Get(testServer.URL + "/")
			require.NoError(t, err)
			require.NoError(t, errorOccurred)

			defer res.Body.Close()

			require.Equal(t, tc.statusCode, res.StatusCode)
			require.Equal(t, "application/json", res.Header.Get("Content-Type"))

			body, err := io.ReadAll(res.Body)
			require.NoError(t, err)

			if tc.statusCode == http.StatusOK {
				require.Equal(t, tc.expected, body)
				return
			}

			var errResponse ErrorResponse
			require.NoError(t, json.Unmarshal(body, &errResponse))
			require.Equal(t, tc.statusCode, errResponse.Status)
			require.NotZero(t, errResponse.Msg)
		})
	}
}

func TestDecodeJSONRequestBody(t *testing.T) {
	t.Run("invalidJSON", func(t *testing.T) {
		responseRecoder := httptest.NewRecorder()
		var dest map[string]int
		require.False(t, DecodeJSONRequestBody(io.NopCloser(bytes.NewReader([]byte(`{"x":1`))), &dest, responseRecoder))
		require.Equal(t, http.StatusBadRequest, responseRecoder.Code)

		var errResponse ErrorResponse
		require.NoError(t, json.NewDecoder(responseRecoder.Body).Decode(&errResponse))
		require.Equal(t, "invalid request body", errResponse.Msg)
		require.Equal(t, http.StatusBadRequest, errResponse.Status)
	})

	t.Run("validJSON", func(t *testing.T) {
		var dest map[string]int
		require.True(t, DecodeJSONRequestBody(io.NopCloser(bytes.NewReader([]byte(`{"x":1}`))), &dest,
			httptest.NewRecorder()))
		require.Equal(t, map[string]int{"x": 1}, dest)
	})
}
