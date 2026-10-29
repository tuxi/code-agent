package model

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OpenCode sometimes forwards the upstream wait only in its error message.
var retryAfterMessage = regexp.MustCompile(`(?i)\b(?:retry\s+(?:after|in)|after)\s+([0-9]+(?:\.[0-9]+)?)\s*(milliseconds?|ms|seconds?|secs?|s|minutes?|mins?|m)\b`)

func withRetryAfter(apiErr *APIError, header http.Header) *APIError {
	if delay, ok := parseRetryAfter(header.Get("Retry-After"), time.Now()); ok {
		apiErr.RetryAfter = delay
		return apiErr
	}
	match := retryAfterMessage.FindStringSubmatch(apiErr.Message)
	if match == nil {
		return apiErr
	}
	unit := "s"
	switch strings.ToLower(match[2]) {
	case "ms", "millisecond", "milliseconds":
		unit = "ms"
	case "m", "min", "mins", "minute", "minutes":
		unit = "m"
	}
	if delay, err := time.ParseDuration(match[1] + unit); err == nil {
		apiErr.RetryAfter = delay
	}
	return apiErr
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds > uint64((1<<63-1)/int64(time.Second)) {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, date.Sub(now)), true
	}
	return 0, false
}

func providerRetryDelay(err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return max(0, apiErr.RetryAfter)
	}
	return 0
}
