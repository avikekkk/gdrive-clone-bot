// Package config loads and validates bot configuration from the environment.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// DefaultSearchRedact is used when SEARCH_REDACT_SECONDS is not set.
const DefaultSearchRedact = 300 * time.Second

// DefaultDatabasePath is used when DATABASE_PATH is not set.
const DefaultDatabasePath = "clonebot.db"

// Error is returned when required environment configuration is missing or invalid.
type Error struct {
	msg string
}

func (e *Error) Error() string { return e.msg }

func configErrorf(format string, args ...any) error {
	return &Error{msg: fmt.Sprintf(format, args...)}
}

// OAuthCredentials is a single installed-app OAuth identity used as Drive auth.
type OAuthCredentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

// Bot holds every setting the bot needs to run.
type Bot struct {
	TelegramAPIID     int
	TelegramAPIHash   string
	TelegramBotToken  string
	OwnerID           int64
	AuthorizedChatIDs []int64
	DestinationID     string

	// SearchRedact is how long a search result message stays visible before it
	// is redacted automatically. Zero disables auto-redaction.
	SearchRedact time.Duration

	// DatabasePath is the SQLite file holding runtime authorizations.
	DatabasePath string

	// ServiceAccounts holds inline JSON credential documents or paths to them.
	ServiceAccounts []string
	// OAuthCredentials are tried after every service account.
	OAuthCredentials []OAuthCredentials
}

// IdentityCount is how many Google identities are configured.
func (c *Bot) IdentityCount() int {
	return len(c.ServiceAccounts) + len(c.OAuthCredentials)
}

// IsAuthorizedChat reports whether the chat is allowed to run /c and /s.
func (c *Bot) IsAuthorizedChat(chatID int64) bool {
	for _, id := range c.AuthorizedChatIDs {
		if id == chatID {
			return true
		}
	}
	return false
}

func clean(value string) string { return strings.TrimSpace(value) }

func env(name string) string { return clean(os.Getenv(name)) }

func normalizeServiceAccountEntry(value any, envName string) (string, error) {
	switch v := value.(type) {
	case map[string]any:
		encoded, err := json.Marshal(v)
		if err != nil {
			return "", configErrorf("%s contains an unencodable service account entry", envName)
		}
		return string(encoded), nil
	case string:
		entry := strings.TrimSpace(v)
		if entry == "" {
			return "", configErrorf("%s contains an empty service account entry", envName)
		}
		return entry, nil
	default:
		return "", configErrorf("%s entries must be JSON objects or strings", envName)
	}
}

// parseServiceAccountEntries accepts a JSON array of objects or paths, a single
// inline JSON object, or a newline-separated list of paths.
func parseServiceAccountEntries(envName, value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}

	if strings.HasPrefix(value, "[") {
		var parsed []any
		if err := json.Unmarshal([]byte(value), &parsed); err != nil {
			return nil, configErrorf("%s must be a valid JSON array", envName)
		}
		entries := make([]string, 0, len(parsed))
		for _, item := range parsed {
			entry, err := normalizeServiceAccountEntry(item, envName)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}
		return entries, nil
	}

	if strings.HasPrefix(value, "{") {
		entry, err := normalizeServiceAccountEntry(value, envName)
		if err != nil {
			return nil, err
		}
		return []string{entry}, nil
	}

	var entries []string
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		entry, err := normalizeServiceAccountEntry(line, envName)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func validateServiceAccountEntry(value, envName string) error {
	if strings.HasPrefix(value, "{") {
		if !json.Valid([]byte(value)) {
			return configErrorf("%s must contain valid JSON credentials", envName)
		}
		return nil
	}
	if _, err := os.Stat(value); err != nil {
		return configErrorf("%s path does not exist. Use absolute or project-relative path.", envName)
	}
	return nil
}

func parseOAuthCredentials(value string) ([]OAuthCredentials, error) {
	if value == "" {
		return nil, nil
	}

	var parsed []map[string]any
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return nil, configErrorf("GOOGLE_OAUTH_CREDENTIALS must be a valid JSON array")
	}

	credentials := make([]OAuthCredentials, 0, len(parsed))
	for idx, item := range parsed {
		if item == nil {
			return nil, configErrorf("GOOGLE_OAUTH_CREDENTIALS entry #%d must be an object", idx+1)
		}
		stringField := func(key string) string {
			raw, ok := item[key].(string)
			if !ok {
				return ""
			}
			return clean(raw)
		}

		clientID := stringField("client_id")
		clientSecret := stringField("client_secret")
		refreshToken := stringField("refresh_token")
		if clientID == "" || clientSecret == "" || refreshToken == "" {
			return nil, configErrorf(
				"GOOGLE_OAUTH_CREDENTIALS entry #%d must include client_id, client_secret, and refresh_token",
				idx+1,
			)
		}

		credentials = append(credentials, OAuthCredentials{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RefreshToken: refreshToken,
		})
	}
	return credentials, nil
}

// parseSearchRedact reads a whole number of seconds; 0 disables auto-redaction.
func parseSearchRedact(value string) (time.Duration, error) {
	if value == "" {
		return DefaultSearchRedact, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0, configErrorf("SEARCH_REDACT_SECONDS must be a non-negative number of seconds")
	}
	return time.Duration(seconds) * time.Second, nil
}

func parseAuthorizedChatIDs(value string) ([]int64, error) {
	if value == "" {
		return nil, nil
	}

	var chatIDs []int64
	for _, part := range strings.Split(strings.ReplaceAll(value, "\n", ","), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, configErrorf("AUTHORIZED_CHAT_IDS must be a comma-separated list of chat IDs")
		}
		chatIDs = append(chatIDs, id)
	}
	return chatIDs, nil
}

// Load reads .env plus the process environment and returns a validated config.
func Load() (*Bot, error) {
	// A missing .env is fine when the environment is already populated.
	_ = godotenv.Load()

	telegramAPIIDRaw := env("TELEGRAM_API_ID")
	telegramAPIHash := env("TELEGRAM_API_HASH")
	telegramBotToken := env("TELEGRAM_BOT_TOKEN")
	ownerIDRaw := env("OWNER_ID")
	destinationID := env("GOOGLE_DRIVE_DESTINATION_ID")

	if telegramAPIIDRaw == "" {
		return nil, configErrorf("Missing TELEGRAM_API_ID in .env")
	}
	if telegramAPIHash == "" {
		return nil, configErrorf("Missing TELEGRAM_API_HASH in .env")
	}
	if telegramBotToken == "" {
		return nil, configErrorf("Missing TELEGRAM_BOT_TOKEN in .env")
	}
	if ownerIDRaw == "" {
		return nil, configErrorf("Missing OWNER_ID in .env")
	}
	if destinationID == "" {
		return nil, configErrorf("Missing GOOGLE_DRIVE_DESTINATION_ID in .env")
	}

	telegramAPIID, err := strconv.Atoi(telegramAPIIDRaw)
	if err != nil {
		return nil, configErrorf("TELEGRAM_API_ID must be an integer")
	}

	ownerID, err := strconv.ParseInt(ownerIDRaw, 10, 64)
	if err != nil {
		return nil, configErrorf("OWNER_ID must be a Telegram numeric user ID")
	}

	authorizedChatIDs, err := parseAuthorizedChatIDs(env("AUTHORIZED_CHAT_IDS"))
	if err != nil {
		return nil, err
	}

	searchRedact, err := parseSearchRedact(env("SEARCH_REDACT_SECONDS"))
	if err != nil {
		return nil, err
	}

	databasePath := env("DATABASE_PATH")
	if databasePath == "" {
		databasePath = DefaultDatabasePath
	}

	primary, err := parseServiceAccountEntries("SERVICE_ACCOUNT_JSON", env("SERVICE_ACCOUNT_JSON"))
	if err != nil {
		return nil, err
	}
	secondary, err := parseServiceAccountEntries("SERVICE_ACCOUNT_JSONS", env("SERVICE_ACCOUNT_JSONS"))
	if err != nil {
		return nil, err
	}
	serviceAccounts := append(primary, secondary...)

	oauthCredentials, err := parseOAuthCredentials(env("GOOGLE_OAUTH_CREDENTIALS"))
	if err != nil {
		return nil, err
	}

	googleClientID := env("GOOGLE_CLIENT_ID")
	googleClientSecret := env("GOOGLE_CLIENT_SECRET")
	googleRefreshToken := env("GOOGLE_REFRESH_TOKEN")
	if googleClientID != "" || googleClientSecret != "" || googleRefreshToken != "" {
		if googleClientID == "" || googleClientSecret == "" || googleRefreshToken == "" {
			return nil, configErrorf(
				"GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, and GOOGLE_REFRESH_TOKEN must be provided together",
			)
		}
		// The simple single-account fields take priority over the JSON array.
		oauthCredentials = append([]OAuthCredentials{{
			ClientID:     googleClientID,
			ClientSecret: googleClientSecret,
			RefreshToken: googleRefreshToken,
		}}, oauthCredentials...)
	}

	if len(serviceAccounts) == 0 && len(oauthCredentials) == 0 {
		return nil, configErrorf(
			"Provide SERVICE_ACCOUNT_JSON and/or GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET/GOOGLE_REFRESH_TOKEN or GOOGLE_OAUTH_CREDENTIALS",
		)
	}

	// Service account entries accept either inline JSON or a file path.
	for idx, entry := range serviceAccounts {
		if err := validateServiceAccountEntry(entry, fmt.Sprintf("service account #%d", idx+1)); err != nil {
			return nil, err
		}
	}

	return &Bot{
		TelegramAPIID:     telegramAPIID,
		TelegramAPIHash:   telegramAPIHash,
		TelegramBotToken:  telegramBotToken,
		OwnerID:           ownerID,
		AuthorizedChatIDs: authorizedChatIDs,
		DestinationID:     destinationID,
		SearchRedact:      searchRedact,
		DatabasePath:      databasePath,
		ServiceAccounts:   serviceAccounts,
		OAuthCredentials:  oauthCredentials,
	}, nil
}
