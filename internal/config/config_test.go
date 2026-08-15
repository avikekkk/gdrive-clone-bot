package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestParseAuthorizedChatIDs(t *testing.T) {
	got, err := parseAuthorizedChatIDs("-1001234567890, -1009876543210\n42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int64{-1001234567890, -1009876543210, 42}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseAuthorizedChatIDs = %v, want %v", got, want)
	}

	if _, err := parseAuthorizedChatIDs("not-an-id"); err == nil {
		t.Error("expected an error for a non-numeric chat ID")
	}
}

func TestParseSearchRedact(t *testing.T) {
	cases := map[string]time.Duration{
		"":     DefaultSearchRedact,
		"300":  300 * time.Second,
		"0":    0,
		"1800": 30 * time.Minute,
	}
	for input, want := range cases {
		got, err := parseSearchRedact(input)
		if err != nil {
			t.Errorf("parseSearchRedact(%q) returned error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("parseSearchRedact(%q) = %v, want %v", input, got, want)
		}
	}

	for _, input := range []string{"-1", "abc", "5m"} {
		if _, err := parseSearchRedact(input); err == nil {
			t.Errorf("parseSearchRedact(%q) should have failed", input)
		}
	}
}

func TestParseServiceAccountEntries(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  []string
	}{
		{"single path", "./service-account.json", []string{"./service-account.json"}},
		{"path array", `["./a.json","./b.json"]`, []string{"./a.json", "./b.json"}},
		{"inline object", `{"type":"service_account"}`, []string{`{"type":"service_account"}`}},
		{"newline separated", "./a.json\n./b.json", []string{"./a.json", "./b.json"}},
		{"empty", "", nil},
	}

	for _, tc := range cases {
		got, err := parseServiceAccountEntries("SERVICE_ACCOUNT_JSON", tc.value)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}

	// An array of inline credential objects is re-encoded compactly.
	got, err := parseServiceAccountEntries("SERVICE_ACCOUNT_JSON", `[{"type": "service_account"}]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != `{"type":"service_account"}` {
		t.Errorf("inline object array = %v", got)
	}

	if _, err := parseServiceAccountEntries("SERVICE_ACCOUNT_JSON", `[oops`); err == nil {
		t.Error("expected an error for a malformed JSON array")
	}
}

func TestValidateServiceAccountEntry(t *testing.T) {
	if err := validateServiceAccountEntry(`{"type":"service_account"}`, "sa"); err != nil {
		t.Errorf("valid inline JSON rejected: %v", err)
	}
	if err := validateServiceAccountEntry(`{"broken"`, "sa"); err == nil {
		t.Error("malformed inline JSON should be rejected")
	}
	if err := validateServiceAccountEntry("/definitely/missing.json", "sa"); err == nil {
		t.Error("missing path should be rejected")
	}

	path := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateServiceAccountEntry(path, "sa"); err != nil {
		t.Errorf("existing path rejected: %v", err)
	}
}

func TestParseOAuthCredentials(t *testing.T) {
	got, err := parseOAuthCredentials(`[{"client_id":"id","client_secret":"secret","refresh_token":"token"}]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []OAuthCredentials{{ClientID: "id", ClientSecret: "secret", RefreshToken: "token"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseOAuthCredentials = %v, want %v", got, want)
	}

	if _, err := parseOAuthCredentials(`[{"client_id":"id"}]`); err == nil {
		t.Error("incomplete credentials should be rejected")
	}
	if _, err := parseOAuthCredentials(`{"client_id":"id"}`); err == nil {
		t.Error("a non-array should be rejected")
	}
}

func TestIsAuthorizedChat(t *testing.T) {
	cfg := &Bot{AuthorizedChatIDs: []int64{-100123, -100456}}
	if !cfg.IsAuthorizedChat(-100456) {
		t.Error("configured chat should be authorized")
	}
	if cfg.IsAuthorizedChat(-100789) {
		t.Error("unconfigured chat should not be authorized")
	}
}

func TestLoadRequiresTelegramSettings(t *testing.T) {
	for _, name := range []string{
		"TELEGRAM_API_ID", "TELEGRAM_API_HASH", "TELEGRAM_BOT_TOKEN", "OWNER_ID",
		"GOOGLE_DRIVE_DESTINATION_ID", "SERVICE_ACCOUNT_JSON", "SERVICE_ACCOUNT_JSONS",
		"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "GOOGLE_REFRESH_TOKEN",
		"GOOGLE_OAUTH_CREDENTIALS", "AUTHORIZED_CHAT_IDS",
	} {
		t.Setenv(name, "")
	}
	// Load() reads .env from the working directory; run somewhere without one.
	t.Chdir(t.TempDir())

	if _, err := Load(); err == nil {
		t.Fatal("Load() should fail without any configuration")
	}
}
