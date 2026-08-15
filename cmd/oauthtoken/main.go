// Command oauthtoken runs the Google OAuth consent flow and prints the .env
// lines for the Drive fallback identity.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"
	drivev3 "google.golang.org/api/drive/v3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate refresh token: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	clientSecretFile := flag.String("client-secret-file", "client_secret.json",
		"Path to the OAuth desktop client JSON downloaded from Google Cloud.")
	clientID := flag.String("client-id", "",
		"OAuth client ID. Use this with -client-secret if you do not have a JSON file.")
	clientSecret := flag.String("client-secret", "",
		"OAuth client secret. Use this with -client-id if you do not have a JSON file.")
	port := flag.Int("port", 0, "Local callback port. Use 0 to pick a free port automatically.")
	flag.Parse()

	conf, err := buildConfig(*clientSecretFile, *clientID, *clientSecret)
	if err != nil {
		return err
	}

	token, err := runLocalServerFlow(conf, *port)
	if err != nil {
		return err
	}
	if token.RefreshToken == "" {
		return errors.New("Google did not return a refresh token. Re-run the same command and " +
			"make sure you approve the consent prompt.")
	}

	fmt.Println()
	fmt.Println("Add these lines to .env:")
	fmt.Printf("GOOGLE_CLIENT_ID=%s\n", conf.ClientID)
	fmt.Printf("GOOGLE_CLIENT_SECRET=%s\n", conf.ClientSecret)
	fmt.Printf("GOOGLE_REFRESH_TOKEN=%s\n", token.RefreshToken)
	return nil
}

// clientSecretFile mirrors the shape of Google's downloaded client JSON.
type clientSecretFile struct {
	Installed *struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	} `json:"installed"`
	Web *struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	} `json:"web"`
}

func buildConfig(secretPath, clientID, clientSecret string) (*oauth2.Config, error) {
	if clientID != "" || clientSecret != "" {
		if clientID == "" || clientSecret == "" {
			return nil, errors.New("-client-id and -client-secret must be provided together")
		}
		return &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     googleoauth.Endpoint,
			Scopes:       []string{drivev3.DriveScope},
		}, nil
	}

	data, err := os.ReadFile(secretPath)
	if err != nil {
		return nil, fmt.Errorf("%s does not exist. Download an OAuth desktop client JSON from "+
			"Google Cloud or pass -client-id and -client-secret", secretPath)
	}

	var parsed clientSecretFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON", secretPath)
	}

	creds := parsed.Installed
	if creds == nil {
		creds = parsed.Web
	}
	if creds == nil || creds.ClientID == "" {
		return nil, fmt.Errorf("%s does not look like a Google OAuth client secret JSON file", secretPath)
	}

	return &oauth2.Config{
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
		Endpoint:     googleoauth.Endpoint,
		Scopes:       []string{drivev3.DriveScope},
	}, nil
}

// runLocalServerFlow serves the OAuth redirect locally and waits for the code.
func runLocalServerFlow(conf *oauth2.Config, port int) (*oauth2.Token, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen on callback port: %w", err)
	}
	defer listener.Close()

	conf.RedirectURL = fmt.Sprintf("http://localhost:%d/", listener.Addr().(*net.TCPAddr).Port)

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, err
	}
	state := hex.EncodeToString(stateBytes)

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			query := r.URL.Query()
			if errMsg := query.Get("error"); errMsg != "" {
				http.Error(w, "Authorization failed: "+errMsg, http.StatusBadRequest)
				results <- result{err: fmt.Errorf("authorization denied: %s", errMsg)}
				return
			}
			if query.Get("state") != state {
				http.Error(w, "State mismatch.", http.StatusBadRequest)
				results <- result{err: errors.New("state mismatch in OAuth callback")}
				return
			}
			code := query.Get("code")
			if code == "" {
				http.Error(w, "Missing authorization code.", http.StatusBadRequest)
				return
			}
			fmt.Fprintln(w, "Authorization complete. You can close this window.")
			results <- result{code: code}
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	authURL := conf.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
	fmt.Println("Open this URL to authorize:")
	fmt.Println(authURL)
	openBrowser(authURL)

	res := <-results
	if res.err != nil {
		return nil, res.err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return conf.Exchange(ctx, res.code)
}

// openBrowser is best effort; the URL is printed either way.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
