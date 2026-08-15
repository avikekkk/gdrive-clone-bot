package drive

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

// Error is an expected clone failure carrying a user-facing message.
type Error struct {
	msg string
}

func (e *Error) Error() string { return e.msg }

func newError(format string, args ...any) *Error {
	return &Error{msg: fmt.Sprintf(format, args...)}
}

// DuplicateError reports that the destination already holds an item of the same name.
type DuplicateError struct {
	Existing Existing
}

func (e *DuplicateError) Error() string { return "FILE EXISTS" }

// Existing describes the item already present at the destination.
type Existing struct {
	ID   string
	Name string
	URL  string
}

func newDuplicateError(existing Existing) *DuplicateError {
	return &DuplicateError{Existing: existing}
}

func asDuplicate(err error, target **DuplicateError) bool { return errors.As(err, target) }

func asDriveError(err error, target **Error) bool { return errors.As(err, target) }

// httpStatus returns the HTTP status of a Google API error, or 0.
func httpStatus(err error) int {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return 0
}

func asGoogleAPIError(err error) (*googleapi.Error, bool) {
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// isAuthRefreshError reports whether the error came from the OAuth token exchange.
func isAuthRefreshError(err error) bool {
	var retrieveErr *oauth2.RetrieveError
	return errors.As(err, &retrieveErr)
}

// httpErrorReason extracts the machine-readable reason, falling back to the message.
func httpErrorReason(apiErr *googleapi.Error) string {
	if len(apiErr.Errors) > 0 && apiErr.Errors[0].Reason != "" {
		return apiErr.Errors[0].Reason
	}
	return apiErr.Message
}

// httpErrorMessage is the human-readable reason, mirroring HttpError._get_reason().
func httpErrorMessage(apiErr *googleapi.Error) string {
	if apiErr.Message != "" {
		return apiErr.Message
	}
	if len(apiErr.Errors) > 0 && apiErr.Errors[0].Message != "" {
		return apiErr.Errors[0].Message
	}
	return "Google Drive API error"
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

// isRetryableHTTPError covers transient failures plus rate-limit flavored 403s.
func isRetryableHTTPError(err error) bool {
	apiErr, ok := asGoogleAPIError(err)
	if !ok {
		return false
	}
	switch apiErr.Code {
	case 429, 500, 502, 503, 504:
		return true
	case 403:
	default:
		return false
	}

	reason := strings.ToLower(httpErrorReason(apiErr))
	message := strings.ToLower(httpErrorMessage(apiErr))
	retryable := []string{
		"ratelimitexceeded",
		"userratelimitexceeded",
		"sharingratelimitexceeded",
		"backenderror",
		"too many requests",
		"queries per minute",
	}
	for _, token := range retryable {
		if strings.Contains(reason, token) || strings.Contains(message, token) {
			return true
		}
	}
	return false
}

// isCopyAuthRotationError reports a per-account quota failure worth retrying
// under the next configured identity.
func isCopyAuthRotationError(err error) bool {
	apiErr, ok := asGoogleAPIError(err)
	if !ok || apiErr.Code != 403 {
		return false
	}
	reason := httpErrorReason(apiErr)
	if reason == "" {
		reason = httpErrorMessage(apiErr)
	}
	normalized := strings.ToLower(reason)
	return containsAny(normalized, "userratelimitexceeded", "dailylimitexceeded", "ratelimitexceeded")
}

// normalizeHTTPError converts a Drive API error into a user-facing Error.
func normalizeHTTPError(err error) error {
	apiErr, ok := asGoogleAPIError(err)
	if !ok {
		if isAuthRefreshError(err) {
			return newError("Google auth failed. Check the configured credentials.")
		}
		return err
	}

	message := httpErrorMessage(apiErr)
	lower := strings.ToLower(message)

	switch apiErr.Code {
	case 401:
		return newError("Google auth failed. Check the configured credentials.")
	case 404:
		return newError("Source item was not found or is not shared with the active account.")
	case 429:
		return newError("Rate limit exceeded. Wait a bit and retry.")
	case 403:
		if containsAny(lower,
			"ratelimitexceeded", "userratelimitexceeded", "sharingratelimitexceeded",
			"too many requests", "queries per minute",
		) {
			return newError("Rate limit exceeded. Wait a bit and retry.")
		}
		if containsAny(lower, "dailylimitexceeded", "daily limit") {
			return newError("Drive API daily limit exceeded. Retry after the quota resets.")
		}
		if strings.Contains(lower, "storagequotaexceeded") ||
			(strings.Contains(lower, "storage") && strings.Contains(lower, "quota")) {
			return newError("Destination storage quota exceeded. Free space and retry.")
		}
		if containsAny(lower, "cannotcopyfile", "copying this file is disabled") {
			return newError("Copy is restricted for this file.")
		}
		return newError("Permission denied (403). For private links, share the source with your " +
			"service account(s)/OAuth user and ensure destination write access.")
	case 400:
		return newError("Bad request to Drive API: %s", message)
	case 500, 502, 503, 504:
		return newError("Google Drive is temporarily unavailable. Retry later.")
	}

	status := "unknown"
	if apiErr.Code != 0 {
		status = fmt.Sprintf("%d", apiErr.Code)
	}
	return newError("Drive API error (%s): %s", status, message)
}
