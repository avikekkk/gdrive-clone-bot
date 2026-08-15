// Package drive clones, searches, and deletes Google Drive items using
// server-side copy across one or more configured Google identities.
package drive

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"
	drivev3 "google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"github.com/avisek/gdrive-clone-bot/internal/config"
)

// Drive MIME types the bot special-cases.
const (
	FileMIME     = "application/vnd.google-apps.file"
	FolderMIME   = "application/vnd.google-apps.folder"
	ShortcutMIME = "application/vnd.google-apps.shortcut"
)

const (
	serviceAccountAuthPrefix = "service_account:"
	oauthAuthPrefix          = "oauth:"
	retryAttempts            = 3
)

// metaFields is the metadata projection used for source items everywhere.
const metaFields = "id,name,size,mimeType,webViewLink,quotaBytesUsed,resourceKey,shortcutDetails,trashed"

// Cloner performs Drive operations, transparently falling back across the
// configured service accounts and OAuth users.
type Cloner struct {
	ctx context.Context
	cfg *config.Bot

	services  map[string]*drivev3.Service
	authOrder []string
	preferred string

	// AuthMode is the identity currently in use.
	AuthMode string
	svc      *drivev3.Service
}

// New builds a Cloner. When preferredAuth is non-empty that identity is tried
// first; it must be one of the configured modes.
func New(ctx context.Context, cfg *config.Bot, preferredAuth string) (*Cloner, error) {
	authOrder := availableAuthModes(cfg)
	if len(authOrder) == 0 {
		return nil, newError("No Google auth method is configured")
	}

	if preferredAuth != "" {
		found := false
		for _, mode := range authOrder {
			if mode == preferredAuth {
				found = true
				break
			}
		}
		if !found {
			return nil, newError("Configured Google auth is missing: %s", preferredAuth)
		}
		reordered := []string{preferredAuth}
		for _, mode := range authOrder {
			if mode != preferredAuth {
				reordered = append(reordered, mode)
			}
		}
		authOrder = reordered
	}

	return &Cloner{
		ctx:       ctx,
		cfg:       cfg,
		services:  map[string]*drivev3.Service{},
		authOrder: authOrder,
		preferred: preferredAuth,
		AuthMode:  authOrder[0],
	}, nil
}

func availableAuthModes(cfg *config.Bot) []string {
	modes := make([]string, 0, len(cfg.ServiceAccounts)+len(cfg.OAuthCredentials))
	for idx := range cfg.ServiceAccounts {
		modes = append(modes, serviceAccountAuthPrefix+strconv.Itoa(idx+1))
	}
	for idx := range cfg.OAuthCredentials {
		modes = append(modes, oauthAuthPrefix+strconv.Itoa(idx+1))
	}
	return modes
}

func (c *Cloner) getService(authMode string) (*drivev3.Service, error) {
	if svc, ok := c.services[authMode]; ok {
		return svc, nil
	}
	svc, err := c.buildService(authMode)
	if err != nil {
		return nil, err
	}
	c.services[authMode] = svc
	return svc, nil
}

func (c *Cloner) buildService(authMode string) (*drivev3.Service, error) {
	switch {
	case authMode == "service_account" || strings.HasPrefix(authMode, serviceAccountAuthPrefix):
		entry, err := c.serviceAccountForMode(authMode)
		if err != nil {
			return nil, err
		}
		data, err := readCredentialJSON(entry)
		if err != nil {
			return nil, err
		}
		creds, err := googleoauth.CredentialsFromJSON(c.ctx, data, drivev3.DriveScope)
		if err != nil {
			return nil, newError("Google auth failed. Check the configured credentials.")
		}
		return drivev3.NewService(c.ctx, option.WithCredentials(creds))

	case authMode == "oauth" || strings.HasPrefix(authMode, oauthAuthPrefix):
		creds, err := c.oauthCredentialsForMode(authMode)
		if err != nil {
			return nil, err
		}
		conf := &oauth2.Config{
			ClientID:     creds.ClientID,
			ClientSecret: creds.ClientSecret,
			Endpoint:     googleoauth.Endpoint,
			Scopes:       []string{drivev3.DriveScope},
		}
		source := conf.TokenSource(c.ctx, &oauth2.Token{RefreshToken: creds.RefreshToken})
		// Refresh eagerly so bad credentials surface here rather than mid-clone.
		if _, err := source.Token(); err != nil {
			return nil, err
		}
		return drivev3.NewService(c.ctx, option.WithTokenSource(source))
	}

	return nil, newError("Unknown Google auth mode: %s", authMode)
}

// readCredentialJSON accepts either an inline JSON document or a path to one.
func readCredentialJSON(entry string) ([]byte, error) {
	if strings.HasPrefix(strings.TrimSpace(entry), "{") {
		return []byte(entry), nil
	}
	data, err := os.ReadFile(entry)
	if err != nil {
		return nil, newError("Cannot read service account credentials: %s", entry)
	}
	return data, nil
}

func (c *Cloner) serviceAccountForMode(authMode string) (string, error) {
	if authMode == "service_account" {
		if len(c.cfg.ServiceAccounts) == 0 {
			return "", newError("No service account is configured")
		}
		return c.cfg.ServiceAccounts[0], nil
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(authMode, serviceAccountAuthPrefix))
	if err != nil || idx < 1 || idx > len(c.cfg.ServiceAccounts) {
		return "", newError("Unknown Google auth mode: %s", authMode)
	}
	return c.cfg.ServiceAccounts[idx-1], nil
}

func (c *Cloner) oauthCredentialsForMode(authMode string) (config.OAuthCredentials, error) {
	if authMode == "oauth" {
		if len(c.cfg.OAuthCredentials) == 0 {
			return config.OAuthCredentials{}, newError("No OAuth credentials are configured")
		}
		return c.cfg.OAuthCredentials[0], nil
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(authMode, oauthAuthPrefix))
	if err != nil || idx < 1 || idx > len(c.cfg.OAuthCredentials) {
		return config.OAuthCredentials{}, newError("Unknown Google auth mode: %s", authMode)
	}
	return c.cfg.OAuthCredentials[idx-1], nil
}

// retryDo re-runs a Drive call for transient failures with a bounded backoff.
func retryDo[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		res, err := fn()
		if err == nil {
			return res, nil
		}
		if !isRetryableHTTPError(err) || attempt == retryAttempts-1 {
			return zero, err
		}
		lastErr = err

		delay := 2 * (attempt + 1)
		if delay > 6 {
			delay = 6
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(time.Duration(delay) * time.Second):
		}
	}
	if lastErr != nil {
		return zero, lastErr
	}
	return zero, newError("Drive API request failed")
}

// withAuthFallback runs op under each configured identity until one succeeds or
// the failure is not the kind another identity could fix.
func withAuthFallback[T any](c *Cloner, op func() (T, error)) (T, error) {
	var zero T
	var lastErr error

	for _, authMode := range c.authOrder {
		c.AuthMode = authMode

		svc, err := c.getService(authMode)
		if err != nil {
			if !c.shouldTryNextAuth(err) {
				return zero, authErrorFor(err)
			}
			lastErr = authErrorFor(err)
			continue
		}
		c.svc = svc

		res, err := op()
		if err == nil {
			return res, nil
		}

		var dup *DuplicateError
		if asDuplicate(err, &dup) {
			return zero, err
		}
		if !c.shouldTryNextAuth(err) {
			return zero, authErrorFor(err)
		}
		lastErr = authErrorFor(err)
	}

	if lastErr != nil {
		return zero, lastErr
	}
	return zero, newError("No Google auth method is configured")
}

// authErrorFor converts token-refresh failures into a user-facing message and
// leaves every other error untouched.
func authErrorFor(err error) error {
	if isAuthRefreshError(err) {
		return newError("Google auth failed. Check the configured credentials.")
	}
	return err
}

// shouldTryNextAuth reports whether another identity might succeed where this
// one failed. The last identity in the order never falls through.
func (c *Cloner) shouldTryNextAuth(err error) bool {
	if c.AuthMode == c.authOrder[len(c.authOrder)-1] {
		return false
	}
	if status := httpStatus(err); status == 403 || status == 404 {
		return true
	}
	if isAuthRefreshError(err) {
		return true
	}
	var driveErr *Error
	if asDriveError(err, &driveErr) {
		msg := strings.ToUpper(driveErr.Error())
		return containsAny(msg,
			"YOU DON'T HAVE PERMS",
			"FILE DOESN'T EXIST",
			"NOT FOUND",
			"GOOGLE AUTH FAILED",
		)
	}
	return false
}

// switchToNextAuth advances to the next identity mid-operation, used when a
// single copy call trips a per-account quota.
func (c *Cloner) switchToNextAuth() bool {
	current := -1
	for i, mode := range c.authOrder {
		if mode == c.AuthMode {
			current = i
			break
		}
	}
	if current < 0 || current >= len(c.authOrder)-1 {
		return false
	}
	next := c.authOrder[current+1]
	svc, err := c.getService(next)
	if err != nil {
		return false
	}
	c.AuthMode = next
	c.svc = svc
	return true
}

// useAuth pins the Cloner to its current identity without any fallback.
func (c *Cloner) useAuth() error {
	svc, err := c.getService(c.AuthMode)
	if err != nil {
		return authErrorFor(err)
	}
	c.svc = svc
	return nil
}

// applyResourceKey attaches a shared-link resource key to a request.
func applyResourceKey(header http.Header, fileID, resourceKey string) {
	if resourceKey == "" {
		return
	}
	header.Set("X-Goog-Drive-Resource-Keys", fileID+"/"+resourceKey)
}
