package httpclient

import (
	"errors"
	"fmt"
	"net/http"
)

type ClientError struct {
	StatusCode int
	Status     string
	Message    string
	Body       []byte
	URL        string
	Method     string
	RequestID  string
}

func (e *ClientError) Error() string {
	msg := fmt.Sprintf("[HTTP %d: %s] %s %s",
		e.StatusCode, http.StatusText(e.StatusCode), e.Method, e.URL)
	if len(e.Body) > 0 {
		msg += " — " + string(e.Body)
	}
	if e.RequestID != "" {
		msg += " (request_id=" + e.RequestID + ")"
	}
	return msg
}

func (e *ClientError) IsClientError() bool {
	return e.StatusCode >= 400 && e.StatusCode < 500
}

func IsHttpError(err error, status int) bool {
	var he *ClientError
	return errors.As(err, &he) && he.StatusCode == status
}

func (e *ClientError) Retryable() bool {
	switch e.StatusCode {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}
