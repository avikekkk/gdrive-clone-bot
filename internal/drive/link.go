package drive

import (
	"regexp"
	"strings"
)

// ParsedLink is a Drive file ID plus the optional resource key from a share link.
type ParsedLink struct {
	FileID      string
	ResourceKey string
}

var (
	fileIDPatterns = []*regexp.Regexp{
		regexp.MustCompile(`/file/d/([a-zA-Z0-9_-]+)`),
		regexp.MustCompile(`/folders/([a-zA-Z0-9_-]+)`),
		regexp.MustCompile(`[?&]id=([a-zA-Z0-9_-]+)`),
	}
	resourceKeyPattern = regexp.MustCompile(`(?i)[?&]resourcekey=([a-zA-Z0-9_-]+)`)
	rawIDPattern       = regexp.MustCompile(`^[a-zA-Z0-9_-]{10,}$`)
)

// ParseLink accepts a Drive file/folder URL or a bare Drive ID.
func ParseLink(link string) (ParsedLink, error) {
	link = strings.TrimSpace(link)
	if rawIDPattern.MatchString(link) {
		return ParsedLink{FileID: link}, nil
	}
	for _, pattern := range fileIDPatterns {
		m := pattern.FindStringSubmatch(link)
		if m == nil {
			continue
		}
		parsed := ParsedLink{FileID: m[1]}
		if rk := resourceKeyPattern.FindStringSubmatch(link); rk != nil {
			parsed.ResourceKey = rk[1]
		}
		return parsed, nil
	}
	return ParsedLink{}, newError("Invalid Google Drive link or ID. Expected a file/folder URL or Drive ID.")
}
