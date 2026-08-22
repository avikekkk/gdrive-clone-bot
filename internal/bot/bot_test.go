package bot

import (
	"strings"
	"testing"
	"time"
)

func TestEncodeDriveIDToken(t *testing.T) {
	// Golden values produced with the same AES-CTR parameters pyaes uses
	// (counter starting at 1), so tokens issued by the Python bot still work.
	cases := map[string]string{
		"1AbCdEfGhIjKlMnOpQrStUvWxYz": "PI47SOmTwLj4/9kLUbUHFusSQ8ikcuKZ+gqA",
		"0B1a2b3c4d5e6f7g":            "PY1oar+0lZyk0oYlC55ePg==",
	}
	for id, want := range cases {
		if got := encodeDriveIDToken(id); got != want {
			t.Errorf("encodeDriveIDToken(%q) = %q, want %q", id, got, want)
		}
		if got := decodeDriveIDToken(want); got != id {
			t.Errorf("decodeDriveIDToken(%q) = %q, want %q", want, got, id)
		}
	}
}

func TestDecodeDriveIDTokenRejectsNonTokens(t *testing.T) {
	for _, input := range []string{
		"",
		"https://drive.google.com/file/d/1AbCdEfGhIjKlMnOpQrStUvWxYz/view",
		"not base64!!",
		"YWJj", // decodes to "abc", too short to be a Drive ID
	} {
		if got := decodeDriveIDToken(input); got != "" {
			t.Errorf("decodeDriveIDToken(%q) = %q, want empty", input, got)
		}
	}
}

func TestParseCommand(t *testing.T) {
	cases := []struct {
		text        string
		wantCommand string
		wantArg     string
		wantPayload string
		wantOK      bool
	}{
		{"/c https://drive.google.com/x", "c", "https://drive.google.com/x", "https://drive.google.com/x", true},
		{"/c@MyCloneBot 1AbCdEfGhIj", "c", "1AbCdEfGhIj", "1AbCdEfGhIj", true},
		{"/s ubuntu server --dir", "s", "ubuntu", "ubuntu server --dir", true},
		{"/START", "start", "", "", true},
		{"  /n  1AbCdEfGhIj  ", "n", "1AbCdEfGhIj", "1AbCdEfGhIj", true},
		{"hello", "", "", "", false},
		{"/", "", "", "", false},
		// A newline after the command still carries the payload: the README
		// documents pasting a batch of IDs one per line.
		{"/c\n1AbCdEfGhIj\n1XyZaBcDeFg", "c", "1AbCdEfGhIj", "1AbCdEfGhIj\n1XyZaBcDeFg", true},
		{"/c\t1AbCdEfGhIj", "c", "1AbCdEfGhIj", "1AbCdEfGhIj", true},
		{"/s@MyCloneBot\nMarvel's Spider-Man 2", "s", "Marvel's", "Marvel's Spider-Man 2", true},
		// Mention and case are both normalized away.
		{"/S@MyCloneBot ubuntu", "s", "ubuntu", "ubuntu", true},
		{"/c", "c", "", "", true},
		{"/@MyCloneBot", "", "", "", false},
	}

	for _, tc := range cases {
		command, args, payload, ok := parseCommand(tc.text)
		if ok != tc.wantOK {
			t.Errorf("parseCommand(%q) ok = %v, want %v", tc.text, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if command != tc.wantCommand {
			t.Errorf("parseCommand(%q) command = %q, want %q", tc.text, command, tc.wantCommand)
		}
		arg := ""
		if len(args) > 0 {
			arg = args[0]
		}
		if arg != tc.wantArg {
			t.Errorf("parseCommand(%q) first arg = %q, want %q", tc.text, arg, tc.wantArg)
		}
		if payload != tc.wantPayload {
			t.Errorf("parseCommand(%q) payload = %q, want %q", tc.text, payload, tc.wantPayload)
		}
	}
}

func TestParseSearchArgs(t *testing.T) {
	cases := []struct {
		payload  string
		query    string
		itemType string
		wantErr  bool
	}{
		{"ubuntu server", "ubuntu server", "files", false},
		{"ubuntu --dir", "ubuntu", "folders", false},
		{"ubuntu --all", "ubuntu", "all", false},
		{`"ubuntu server" iso`, "ubuntu server iso", "files", false},
		{"--dir ubuntu", "", "files", true},
		{"ubuntu --dir --all", "", "files", true},
		{"ubuntu --dir --dir", "", "files", true},
		// An apostrophe is a character in a title, not a quote to be closed.
		{"Marvel's Spider-Man 2", "Marvel's Spider-Man 2", "files", false},
		{"Marvel's Spider-Man 2 --dir", "Marvel's Spider-Man 2", "folders", false},
		{`ubuntu "unclosed`, "ubuntu unclosed", "files", false},
		{"it's a wonderful life", "it's a wonderful life", "files", false},
		{`don't stop 'til you get enough`, `don't stop til you get enough`, "files", false},
		// A trailing backslash used to fail as an unfinished escape.
		{`ubuntu\`, `ubuntu\`, "files", false},
		{`C:\path\to\file`, `C:\path\to\file`, "files", false},
		// Whitespace shapes collapse to the same query.
		{"  ubuntu   server  ", "ubuntu server", "files", false},
		{"ubuntu\tserver\niso", "ubuntu server iso", "files", false},
		// Nothing but a flag, or nothing but quotes, leaves an empty query.
		{"--dir", "", "folders", false},
		{"--all", "", "all", false},
		{`''`, "", "files", false},
		{`" "`, "", "files", false},
		{"", "", "files", false},
		// Unicode and emoji survive intact.
		{"Amélie 日本語 🎬", "Amélie 日本語 🎬", "files", false},
		// A flag-looking word that is not a flag stays part of the query.
		{"--director cut", "--director cut", "files", false},
	}

	for _, tc := range cases {
		query, itemType, parseErr := parseSearchArgs(tc.payload)
		if (parseErr != "") != tc.wantErr {
			t.Errorf("parseSearchArgs(%q) error = %q, wantErr %v", tc.payload, parseErr, tc.wantErr)
			continue
		}
		if tc.wantErr {
			continue
		}
		if query != tc.query || itemType != tc.itemType {
			t.Errorf("parseSearchArgs(%q) = (%q, %q), want (%q, %q)",
				tc.payload, query, itemType, tc.query, tc.itemType)
		}
	}
}

func TestSimpleErrorText(t *testing.T) {
	cases := map[string]string{
		"FILE EXISTS": "File exists",
		"Source item was not found or is not shared with the active account.":      "File not found or not shared",
		"Rate limit exceeded. Wait a bit and retry.":                               "Rate limit exceeded",
		"Drive API daily limit exceeded. Retry after the quota resets.":            "Daily limit exceeded",
		"Destination storage quota exceeded. Free space and retry.":                "Storage quota exceeded",
		"Copy is restricted for this file.":                                        "Copy restricted",
		"Google Drive is temporarily unavailable. Retry later.":                    "Drive temporarily unavailable",
		"Google auth failed. Check the configured credentials.":                    "Google auth failed",
		"Invalid Google Drive link or ID. Expected a file/folder URL or Drive ID.": "Invalid Drive link",
		"YOU DON'T HAVE PERMS": "You don't have perms",
		"":                     "Unexpected error",
	}
	for input, want := range cases {
		if got := simpleErrorText(input); got != want {
			t.Errorf("simpleErrorText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSessionStoreEvictsOldest(t *testing.T) {
	store := newSessionStore(2)
	store.put("a", &searchSession{query: "a"})
	store.put("b", &searchSession{query: "b"})
	store.put("c", &searchSession{query: "c"})

	if _, ok := store.get("a"); ok {
		t.Error("oldest session should have been evicted")
	}
	for _, token := range []string{"b", "c"} {
		if _, ok := store.get(token); !ok {
			t.Errorf("session %q should still be present", token)
		}
	}

	store.remove("b")
	if _, ok := store.get("b"); ok {
		t.Error("removed session should be gone")
	}
}

func TestSources(t *testing.T) {
	token := encodeDriveIDToken("1AbCdEfGhIjKlMnOpQrStUvWxYz")
	cases := []struct {
		payload string
		want    []string
	}{
		{"1AbCdEfGhIjKl", []string{"1AbCdEfGhIjKl"}},
		{"1AbCdEfGhIjKl 1XyZaBcDeFgHi", []string{"1AbCdEfGhIjKl", "1XyZaBcDeFgHi"}},
		{"1AbCdEfGhIjKl,1XyZaBcDeFgHi", []string{"1AbCdEfGhIjKl", "1XyZaBcDeFgHi"}},
		{"1AbCdEfGhIjKl,  1XyZaBcDeFgHi ,, ", []string{"1AbCdEfGhIjKl", "1XyZaBcDeFgHi"}},
		{"1AbCdEfGhIjKl\n1XyZaBcDeFgHi", []string{"1AbCdEfGhIjKl", "1XyZaBcDeFgHi"}},
		// Duplicates are cloned once.
		{"1AbCdEfGhIjKl 1AbCdEfGhIjKl", []string{"1AbCdEfGhIjKl"}},
		// Search-result tokens are decoded to their Drive ID.
		{token, []string{"1AbCdEfGhIjKlMnOpQrStUvWxYz"}},
		{"", nil},
		// A search query typed into /c queues nothing.
		{"a shop for killers --dir", nil},
		// Junk words alongside a real ID leave only the ID.
		{"shop 1AbCdEfGhIjKl --dir", []string{"1AbCdEfGhIjKl"}},
		// Links survive whole: ParseLink still needs the resourcekey a bare ID
		// would drop.
		{"https://drive.google.com/file/d/1AbCdEfGhIjKl/view?usp=sharing",
			[]string{"https://drive.google.com/file/d/1AbCdEfGhIjKl/view?usp=sharing"}},
		{"https://drive.google.com/drive/folders/1AbCdEfGhIjKl",
			[]string{"https://drive.google.com/drive/folders/1AbCdEfGhIjKl"}},
		// Newline-separated pastes, the shape the README documents.
		{"1AbCdEfGhIjKl\n1XyZaBcDeFgHi", []string{"1AbCdEfGhIjKl", "1XyZaBcDeFgHi"}},
		// Punctuation-only and short words never reach the network.
		{"...", nil},
		{"a b c", nil},
		{"🎬", nil},
	}

	for _, tc := range cases {
		r := &request{payload: tc.payload}
		got := r.sources()
		if len(got) != len(tc.want) {
			t.Errorf("sources(%q) = %v, want %v", tc.payload, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("sources(%q) = %v, want %v", tc.payload, got, tc.want)
				break
			}
		}
	}
}

func TestHelpTextEscapesPlaceholders(t *testing.T) {
	// Raw angle brackets would be swallowed by the HTML parse mode.
	if strings.Contains(helpText, "<google_drive") {
		t.Error("help text must escape placeholder angle brackets")
	}
}

func TestReadableTime(t *testing.T) {
	cases := map[time.Duration]string{
		0:                             "0s",
		45 * time.Second:              "45s",
		90 * time.Second:              "1m30s",
		2 * time.Hour:                 "2h",
		25*time.Hour + 30*time.Minute: "1d1h30m",
		26*time.Hour + 3*time.Second:  "1d2h3s",
	}
	for input, want := range cases {
		if got := readableTime(input); got != want {
			t.Errorf("readableTime(%v) = %q, want %q", input, got, want)
		}
	}
}
