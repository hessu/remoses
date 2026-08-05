package client

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError is a non-200 answer from the API.
//
// The status is kept separate from the text so that callers can branch on it.
// That matters most for 401: a monitor that retried a rejected password on a
// backoff loop would hammer the daemon's bcrypt verifier forever and never tell
// the operator what was wrong, so authentication failure has to be
// distinguishable from a transient network error rather than being reported as
// one more thing that did not work.
type APIError struct {
	Status int
	Title  string
	Detail string
	URL    string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s: %d %s", e.URL, e.Status, e.Title)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// IsUnauthorized reports whether err is a 401 from the API.
func IsUnauthorized(err error) bool { return hasStatus(err, http.StatusUnauthorized) }

// IsNotFound reports whether err is a 404 from the API, which for a radio path
// means the instance has no radio with that id.
func IsNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

// Fatal reports whether retrying err could ever succeed. Bad credentials and a
// radio that does not exist will not fix themselves, and a monitor that
// reconnected through them would spin silently instead of saying what is wrong.
func Fatal(err error) bool {
	var e *APIError
	if !errors.As(err, &e) {
		return false
	}
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

func hasStatus(err error, status int) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == status
}
