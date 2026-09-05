package bot

import (
	"context"
	"errors"
	"html"
	"strings"
	"time"

	"github.com/avisek/gdrive-clone-bot/internal/drive"
	"github.com/avisek/gdrive-clone-bot/internal/progress"
)

const progressEditInterval = 2500 * time.Millisecond

// Transient statuses are plain text; only final and per-item states are
// rendered as monospace blocks or headers.
const (
	statusChecking = "Checking"
	statusNuking   = "Nuking"
)

// handleClone clones every source in the command, one after another, so a
// single /c can carry a whole page of search result IDs.
func (r *request) handleClone(ctx context.Context) {
	if r.rejectUnauthorizedClone(ctx) {
		return
	}

	sources := r.sources()
	if len(sources) == 0 {
		r.replyLogged(ctx, "Provide a Drive link, ID, or search result ID")
		return
	}

	b := r.startBatch(ctx, sources)
	if b == nil {
		return
	}
	for i := range b.items {
		if ctx.Err() != nil {
			return
		}
		r.cloneOne(ctx, b, i)
	}
}

func (r *request) cloneOne(ctx context.Context, b *batch, index int) {
	source := b.items[index].source

	log := r.bot.log.With("context", r.context(), "link", source)
	if b.multi() {
		log = log.With("item", itoa(int64(index+1))+"/"+itoa(int64(len(b.items))))
		// Take this item out of the queue display now that its turn has come.
		b.status(ctx, index, statusChecking)
	}
	log.Info("Clone requested")

	cloner, err := drive.New(ctx, r.bot.cfg, "")
	if err != nil {
		log.Warn("Clone precheck failed", "reason", err)
		b.fail(ctx, index, html.EscapeString(simpleErrorText(err.Error())))
		return
	}

	precheck, err := cloner.PrepareClone(source, r.bot.cfg.DestinationID)
	if err != nil {
		r.reportCloneFailure(ctx, log, b, index, err, "precheck")
		return
	}
	log.Info("Clone precheck ok",
		"source_id", precheck.SourceID,
		"name", precheck.Name,
		"mime", precheck.SourceMIMEType,
		"auth", precheck.AuthMode,
	)

	// The precheck knows the real name, so later blocks stop showing the raw ID.
	if precheck.Name != "" {
		b.items[index].name = precheck.Name
	}

	prog := progress.NewClone(precheck.Name)
	onProgress := func(p *progress.Clone, force bool) {
		b.set(ctx, index, p.Message(), force)
	}

	b.set(ctx, index, prog.Message(), true)
	log.Info("Clone started", "destination_id", r.bot.cfg.DestinationID)

	// Reuse the identity that passed the precheck so the copy does not start
	// over from the first service account.
	cloner, err = drive.New(ctx, r.bot.cfg, precheck.AuthMode)
	if err != nil {
		r.reportCloneFailure(ctx, log, b, index, err, "")
		return
	}

	result, err := cloner.Clone(source, r.bot.cfg.DestinationID, prog, onProgress)
	if err != nil {
		r.reportCloneFailure(ctx, log, b, index, err, "")
		return
	}

	log.Info("Clone completed",
		"cloned_id", result.ID,
		"name", result.Name,
		"mime", result.MIMEType,
		"url", result.URL,
	)

	b.set(ctx, index, prog.CompletionMessage(result.Name, result.URL), true)
	log.Info("Clone final message updated", "cloned_id", result.ID, "name", result.Name)
}

// sourceName resolves a display name for a queued item, falling back to the
// raw input when the item cannot be read yet.
func (r *request) sourceName(ctx context.Context, source string) string {
	cloner, err := drive.New(ctx, r.bot.cfg, "")
	if err != nil {
		return source
	}
	meta, err := cloner.GetFileMetadata(source)
	if err != nil || meta.Name == "" {
		return source
	}
	return meta.Name
}

// reportCloneFailure renders the user-facing outcome for a failed clone.
func (r *request) reportCloneFailure(
	ctx context.Context,
	log logger,
	b *batch,
	index int,
	err error,
	stage string,
) {
	label := "Clone failed"
	if stage != "" {
		label = "Clone " + stage + " failed"
	}

	var duplicate *drive.DuplicateError
	if errors.As(err, &duplicate) {
		log.Warn(label+": duplicate",
			"existing_id", duplicate.Existing.ID,
			"name", duplicate.Existing.Name,
			"url", duplicate.Existing.URL,
		)
		b.fail(ctx, index, existsMessage(duplicate.Existing.URL))
		return
	}

	var driveErr *drive.Error
	if errors.As(err, &driveErr) {
		msg := simpleErrorText(driveErr.Error())
		log.Warn(label, "reason", msg)
		b.fail(ctx, index, html.EscapeString(msg))
		return
	}

	if ctx.Err() != nil {
		log.Warn(label, "reason", "shutting down")
		b.fail(ctx, index, errInterrupted)
		return
	}
	log.Error("Unhandled clone failure", "error", err)
	b.fail(ctx, index, errUnexpected)
}

func existsMessage(url string) string {
	if url == "" {
		return errFileExists
	}
	return errFileExists + " <b>•</b> <a href=\"" + html.EscapeString(url) + "\"><b>DL</b></a>"
}

func codeBlock(text string) string {
	return "<code>" + html.EscapeString(text) + "</code>"
}

// handleDelete deletes every target in the command, one after another, so a
// batch of search result IDs can be cleaned up in one go.
func (r *request) handleDelete(ctx context.Context) {
	if r.rejectUnauthorizedClone(ctx) {
		return
	}

	sources := r.sources()
	if len(sources) == 0 {
		r.replyLogged(ctx, "Provide a Drive link, ID, or search result ID")
		return
	}

	b := r.startBatch(ctx, sources)
	if b == nil {
		return
	}
	for i := range b.items {
		if ctx.Err() != nil {
			return
		}
		r.deleteOne(ctx, b, i)
	}
}

func (r *request) deleteOne(ctx context.Context, b *batch, index int) {
	source := b.items[index].source

	log := r.bot.log.With("context", r.context(), "link", source)
	if b.multi() {
		log = log.With("item", itoa(int64(index+1))+"/"+itoa(int64(len(b.items))))
		b.status(ctx, index, statusChecking)
	}
	log.Info("Nuke requested")

	cloner, err := drive.New(ctx, r.bot.cfg, "")
	if err != nil {
		r.reportDeleteFailure(ctx, log, b, index, err, "precheck")
		return
	}

	target, err := cloner.PrepareDelete(source)
	if err != nil {
		r.reportDeleteFailure(ctx, log, b, index, err, "precheck")
		return
	}
	log.Info("Nuke precheck ok",
		"target_id", target.ID,
		"name", target.Name,
		"mode", target.Mode,
		"mime", target.MIMEType,
	)

	// The precheck knows the real name, so later blocks stop showing the raw ID.
	if target.Name != "" {
		b.items[index].name = target.Name
	}
	b.status(ctx, index, statusNuking)

	cloner, err = drive.New(ctx, r.bot.cfg, target.AuthMode)
	if err != nil {
		r.reportDeleteFailure(ctx, log, b, index, err, "")
		return
	}

	result, err := cloner.PerformDelete(target)
	if err != nil {
		r.reportDeleteFailure(ctx, log, b, index, err, "")
		return
	}
	log.Info("Nuke completed",
		"target_id", result.ID,
		"name", result.Name,
		"mode", target.Mode,
		"mime", target.MIMEType,
	)

	b.set(ctx, index, header("NUKED")+"\n"+codeBlock(result.Name), true)
}

func (r *request) reportDeleteFailure(
	ctx context.Context,
	log logger,
	b *batch,
	index int,
	err error,
	stage string,
) {
	label := "Nuke failed"
	if stage != "" {
		label = "Nuke " + stage + " failed"
	}

	var driveErr *drive.Error
	if errors.As(err, &driveErr) {
		msg := simpleErrorText(driveErr.Error())
		log.Warn(label, "reason", msg)
		b.fail(ctx, index, html.EscapeString(msg))
		return
	}

	if ctx.Err() != nil {
		log.Warn(label, "reason", "shutting down")
		b.fail(ctx, index, errInterrupted)
		return
	}
	log.Error("Unhandled nuke failure", "error", err)
	b.fail(ctx, index, errUnexpected)
}

// replyLogged sends a reply, logging rather than propagating a send failure.
func (r *request) replyLogged(ctx context.Context, text string) {
	if _, err := r.reply(ctx, text); err != nil {
		r.bot.log.Warn("Failed to send reply", "error", err)
	}
}

// editLogged edits a message, treating a no-op edit as success.
func (r *request) editLogged(ctx context.Context, msgID int, text string) {
	if err := r.edit(ctx, msgID, text, nil); err != nil && !isNotModified(err) {
		r.bot.log.Warn("Failed to edit message", "error", err)
	}
}

// Error statuses shown to users. The Drive layer keeps its own upper-case
// sentinels, which these are matched against case-insensitively.
const (
	errFileExists       = "File exists"
	errFileNotShared    = "File not found or not shared"
	errFileDoesNotExist = "File doesn't exist"
	errRateLimit        = "Rate limit exceeded"
	errDailyLimit       = "Daily limit exceeded"
	errStorageQuota     = "Storage quota exceeded"
	errCopyRestricted   = "Copy restricted"
	errDriveUnavailable = "Drive temporarily unavailable"
	errAuthFailed       = "Google auth failed"
	errInvalidLink      = "Invalid Drive link"
	errBadRequest       = "Bad Drive request"
	errNoPerms          = "You don't have perms"
	errUnexpected       = "Unexpected error"
	errInterrupted      = "Interrupted by a bot restart, please try again"
)

// simpleErrorText collapses a detailed Drive error into the short status the
// bot shows users.
func simpleErrorText(raw string) string {
	msg := strings.TrimSpace(raw)
	upper := strings.ToUpper(msg)

	switch {
	case containsAny(upper, "FILE EXISTS", "ALREADY EXISTS"):
		return errFileExists
	case containsAny(upper, "NOT FOUND OR NOT SHARED", "NOT SHARED WITH THE ACTIVE ACCOUNT"):
		return errFileNotShared
	case containsAny(upper, "FILE DOESN'T EXIST", "NOT FOUND"):
		return errFileDoesNotExist
	case containsAny(upper, "RATE LIMIT", "TOO MANY REQUESTS", "429"):
		return errRateLimit
	case strings.Contains(upper, "DAILY LIMIT"):
		return errDailyLimit
	case containsAny(upper, "STORAGE QUOTA", "QUOTA EXCEEDED"):
		return errStorageQuota
	case containsAny(upper, "COPY IS RESTRICTED", "COPYING THIS FILE IS DISABLED"):
		return errCopyRestricted
	case containsAny(upper, "TEMPORARILY UNAVAILABLE", " 500", " 502", " 503", " 504"):
		return errDriveUnavailable
	case containsAny(upper, "AUTH FAILED", "INVALID CREDENTIAL", " 401"):
		return errAuthFailed
	case strings.Contains(upper, "INVALID GOOGLE DRIVE LINK"):
		return errInvalidLink
	case containsAny(upper, "BAD REQUEST", " 400"):
		return errBadRequest
	case containsAny(upper, "YOU DON'T HAVE PERMS", "PERMISSION", "403"):
		return errNoPerms
	}

	if msg == "" {
		return errUnexpected
	}
	return msg
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

// logger is the subset of slog used by the command handlers.
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}
