package drive

import (
	"errors"
	"testing"

	drivev3 "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

func TestParseLink(t *testing.T) {
	cases := []struct {
		link        string
		fileID      string
		resourceKey string
	}{
		{"1AbCdEfGhIjKlMnOpQrStUvWxYz", "1AbCdEfGhIjKlMnOpQrStUvWxYz", ""},
		{"https://drive.google.com/file/d/1AbCdEfGhIj/view", "1AbCdEfGhIj", ""},
		{"https://drive.google.com/drive/folders/1FolderIdHere", "1FolderIdHere", ""},
		{"https://drive.google.com/open?id=1OpenIdHere", "1OpenIdHere", ""},
		{"https://drive.google.com/file/d/1AbCdEfGhIj/view?resourcekey=0-abcDEF", "1AbCdEfGhIj", "0-abcDEF"},
		{"https://drive.google.com/file/d/1AbCdEfGhIj/view?resourceKey=0-abcDEF", "1AbCdEfGhIj", "0-abcDEF"},
	}

	for _, tc := range cases {
		parsed, err := ParseLink(tc.link)
		if err != nil {
			t.Errorf("ParseLink(%q) returned error: %v", tc.link, err)
			continue
		}
		if parsed.FileID != tc.fileID || parsed.ResourceKey != tc.resourceKey {
			t.Errorf("ParseLink(%q) = %+v, want id %q key %q", tc.link, parsed, tc.fileID, tc.resourceKey)
		}
	}
}

func TestParseLinkRejectsJunk(t *testing.T) {
	for _, link := range []string{"", "short", "https://example.com/nothing"} {
		if _, err := ParseLink(link); err == nil {
			t.Errorf("ParseLink(%q) should have failed", link)
		}
	}
}

func TestItemSize(t *testing.T) {
	cases := []struct {
		file *drivev3.File
		want int64
	}{
		{&drivev3.File{Size: 1024}, 1024},
		{&drivev3.File{QuotaBytesUsed: 2048}, 2048},
		{&drivev3.File{Size: 1024, QuotaBytesUsed: 2048}, 1024},
		{&drivev3.File{}, 0},
	}
	for _, tc := range cases {
		if got := itemSize(tc.file); got != tc.want {
			t.Errorf("itemSize(%+v) = %d, want %d", tc.file, got, tc.want)
		}
	}
}

func TestEscapeQueryValue(t *testing.T) {
	cases := map[string]string{
		"plain":            "plain",
		"it's":             `it\'s`,
		`back\slash`:       `back\\slash`,
		"  padded  ":       "padded",
		`quote' and \ mix`: `quote\' and \\ mix`,
	}
	for input, want := range cases {
		if got := escapeQueryValue(input); got != want {
			t.Errorf("escapeQueryValue(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDriveURLFor(t *testing.T) {
	cases := []struct {
		file *drivev3.File
		want string
	}{
		{&drivev3.File{Id: "x", WebViewLink: "https://link"}, "https://link"},
		{&drivev3.File{Id: "x", MimeType: FolderMIME}, "https://drive.google.com/drive/folders/x"},
		{&drivev3.File{Id: "x", MimeType: "video/mp4"}, "https://drive.google.com/file/d/x/view"},
	}
	for _, tc := range cases {
		if got := driveURLFor(tc.file); got != tc.want {
			t.Errorf("driveURLFor(%+v) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

func apiError(code int, reason, message string) error {
	return &googleapi.Error{
		Code:    code,
		Message: message,
		Errors:  []googleapi.ErrorItem{{Reason: reason, Message: message}},
	}
}

func TestNormalizeHTTPError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{apiError(401, "authError", "Invalid Credentials"), "Google auth failed. Check the configured credentials."},
		{apiError(404, "notFound", "File not found"), "Source item was not found or is not shared with the active account."},
		{apiError(429, "rateLimitExceeded", "Too many requests"), "Rate limit exceeded. Wait a bit and retry."},
		{apiError(403, "userRateLimitExceeded", "userRateLimitExceeded"), "Rate limit exceeded. Wait a bit and retry."},
		{apiError(403, "dailyLimitExceeded", "dailyLimitExceeded"), "Drive API daily limit exceeded. Retry after the quota resets."},
		{apiError(403, "storageQuotaExceeded", "storageQuotaExceeded"), "Destination storage quota exceeded. Free space and retry."},
		{apiError(403, "cannotCopyFile", "cannotCopyFile"), "Copy is restricted for this file."},
		{apiError(503, "backendError", "Backend Error"), "Google Drive is temporarily unavailable. Retry later."},
	}

	for _, tc := range cases {
		got := normalizeHTTPError(tc.err)
		var driveErr *Error
		if !errors.As(got, &driveErr) {
			t.Errorf("normalizeHTTPError(%v) returned %T, want *Error", tc.err, got)
			continue
		}
		if driveErr.Error() != tc.want {
			t.Errorf("normalizeHTTPError(%v) = %q, want %q", tc.err, driveErr.Error(), tc.want)
		}
	}
}

func TestIsRetryableHTTPError(t *testing.T) {
	retryable := []error{
		apiError(429, "rateLimitExceeded", ""),
		apiError(500, "internalError", ""),
		apiError(503, "backendError", ""),
		apiError(403, "userRateLimitExceeded", ""),
	}
	for _, err := range retryable {
		if !isRetryableHTTPError(err) {
			t.Errorf("isRetryableHTTPError(%v) = false, want true", err)
		}
	}

	notRetryable := []error{
		apiError(404, "notFound", ""),
		apiError(403, "insufficientFilePermissions", ""),
		errors.New("plain error"),
	}
	for _, err := range notRetryable {
		if isRetryableHTTPError(err) {
			t.Errorf("isRetryableHTTPError(%v) = true, want false", err)
		}
	}
}

func TestIsCopyAuthRotationError(t *testing.T) {
	if !isCopyAuthRotationError(apiError(403, "dailyLimitExceeded", "")) {
		t.Error("daily limit 403 should rotate identity")
	}
	if isCopyAuthRotationError(apiError(403, "insufficientFilePermissions", "")) {
		t.Error("permission 403 should not rotate identity")
	}
	if isCopyAuthRotationError(apiError(404, "notFound", "")) {
		t.Error("404 should not rotate identity")
	}
}

func TestSearchItemSize(t *testing.T) {
	size := int64(4096)
	withComputed := &SearchItem{File: &drivev3.File{}, ComputedSize: &size}
	if got, ok := withComputed.Size(); !ok || got != 4096 {
		t.Errorf("Size() = (%d, %v), want (4096, true)", got, ok)
	}

	fromFile := &SearchItem{File: &drivev3.File{QuotaBytesUsed: 10}}
	if got, ok := fromFile.Size(); !ok || got != 10 {
		t.Errorf("Size() = (%d, %v), want (10, true)", got, ok)
	}

	unknown := &SearchItem{File: &drivev3.File{}}
	if _, ok := unknown.Size(); ok {
		t.Error("Size() should report unknown when nothing is known")
	}
}

func TestBuildSearchQuery(t *testing.T) {
	cases := []struct {
		query    string
		itemType string
		want     string
	}{
		{"ubuntu", "files", `name contains 'ubuntu' and trashed=false and mimeType != '` + FolderMIME + `'`},
		{"ubuntu", "folders", `name contains 'ubuntu' and trashed=false and mimeType = '` + FolderMIME + `'`},
		{"ubuntu", "all", `name contains 'ubuntu' and trashed=false`},
		// An apostrophe must be escaped, not left to close the quoted literal.
		{"Marvel's", "all", `name contains 'Marvel\'s' and trashed=false`},
		{"Marvel's Spider-Man 2", "all",
			`name contains 'Marvel\'s' and name contains 'Spider-Man' and name contains '2' and trashed=false`},
		{`back\slash`, "all", `name contains 'back\\slash' and trashed=false`},
		{`quote' and \ mix`, "all",
			`name contains 'quote\'' and name contains 'and' and name contains '\\' and name contains 'mix' and trashed=false`},
	}

	for _, tc := range cases {
		if got := buildSearchQuery(tc.query, tc.itemType); got != tc.want {
			t.Errorf("buildSearchQuery(%q, %q) =\n  %s\nwant\n  %s", tc.query, tc.itemType, got, tc.want)
		}
	}
}

// Every quote inside a built query must be escaped or balanced, which is what
// the Drive API means by "no closing quotation".
func TestBuildSearchQueryQuotesBalance(t *testing.T) {
	queries := []string{
		"Marvel's Spider-Man 2", "it's", "'", "''", "'''", `a'b'c`,
		`don't stop 'til you get enough`, `tra\iling\`, `"double"`, "Amélie 日本語 🎬",
	}
	for _, query := range queries {
		for _, itemType := range []string{"files", "folders", "all"} {
			built := buildSearchQuery(query, itemType)
			open := false
			for i, char := range built {
				if char != '\'' {
					continue
				}
				// Count the backslashes immediately before this quote: an odd
				// number means it is escaped and does not toggle the literal.
				slashes := 0
				for j := i - 1; j >= 0 && built[j] == '\\'; j-- {
					slashes++
				}
				if slashes%2 == 0 {
					open = !open
				}
			}
			if open {
				t.Errorf("buildSearchQuery(%q, %q) leaves an unclosed quote:\n  %s", query, itemType, built)
			}
		}
	}
}
