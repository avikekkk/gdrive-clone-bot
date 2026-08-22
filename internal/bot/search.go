package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"sync"
	"time"

	tdhtml "github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/message/markup"
	messagepeer "github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/tg"

	"github.com/avisek/gdrive-clone-bot/internal/drive"
	"github.com/avisek/gdrive-clone-bot/internal/progress"
)

const (
	searchResultsPerPage = 5
	maxSearchSessions    = 100
	searchLimit          = 50
)

// searchSession keeps one user's result set alive across pagination taps.
type searchSession struct {
	results  []*drive.SearchItem
	query    string
	itemType string
	userID   int64
}

// sessionStore is a bounded FIFO cache of search sessions.
type sessionStore struct {
	mu    sync.Mutex
	max   int
	order []string
	items map[string]*searchSession
}

func newSessionStore(max int) *sessionStore {
	return &sessionStore{max: max, items: map[string]*searchSession{}}
}

func (s *sessionStore) put(token string, session *searchSession) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.items[token] = session
	s.order = append(s.order, token)
	for len(s.order) > s.max {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.items, oldest)
	}
}

func (s *sessionStore) get(token string) (*searchSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.items[token]
	return session, ok
}

func (s *sessionStore) remove(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.items, token)
	for i, existing := range s.order {
		if existing == token {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

func newSessionToken() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(buf)
}

func (r *request) handleSearch(ctx context.Context) {
	if r.rejectUnauthorizedClone(ctx) {
		return
	}

	if r.payload == "" {
		r.replyLogged(ctx, "Provide a query")
		return
	}

	query, itemType, parseErr := parseSearchArgs(r.payload)
	if parseErr != "" {
		r.replyLogged(ctx, html.EscapeString(parseErr))
		return
	}
	if query == "" {
		r.replyLogged(ctx, "Provide a query")
		return
	}

	log := r.bot.log.With("context", r.context(), "query", query, "type", itemType)
	log.Info("Search requested")

	searchingMsgID, err := r.reply(ctx, "Searching")
	if err != nil {
		log.Warn("Failed to post status message", "error", err)
		return
	}

	results, err := r.runSearch(ctx, query, itemType)
	if err != nil {
		var driveErr *drive.Error
		if errors.As(err, &driveErr) {
			msg := simpleErrorText(driveErr.Error())
			log.Warn("Search failed", "reason", msg)
			r.editLogged(ctx, searchingMsgID, html.EscapeString(msg))
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Error("Unhandled search failure", "error", err)
		r.editLogged(ctx, searchingMsgID, errUnexpected)
		return
	}

	if len(results) == 0 {
		log.Info("Search completed", "results", 0)
		r.editLogged(ctx, searchingMsgID,
			header("SEARCH RESULTS")+"\n"+codeBlock(query)+"\n<code>No results</code>")
		return
	}

	token := newSessionToken()
	r.bot.sessions.put(token, &searchSession{
		results:  results,
		query:    query,
		itemType: itemType,
		userID:   r.userID(),
	})

	text, totalPages, page := buildSearchPageText(results, query, 0)
	log.Info("Search completed", "results", len(results))
	if err := r.edit(ctx, searchingMsgID, text, buildSearchPageMarkup(token, page, totalPages)); err != nil {
		log.Warn("Failed to show search results", "error", err)
		return
	}
	r.scheduleRedact(ctx, searchingMsgID, token)
}

// redactedMessage is shown once results are hidden, manually or on a timer.
const redactedMessage = "RESULTS REDACTED"

// clearKeyboard removes the inline keyboard from a message. An edit that sends
// no markup drops the old one; sending an empty ReplyInlineMarkup instead is
// rejected outright with REPLY_MARKUP_INVALID.
func clearKeyboard() tg.ReplyMarkupClass { return nil }

// scheduleRedact hides a results message after SEARCH_REDACT_SECONDS so Drive
// IDs do not linger in chat history.
func (r *request) scheduleRedact(ctx context.Context, msgID int, token string) {
	lifetime := r.bot.cfg.SearchRedact
	if lifetime <= 0 {
		return
	}

	// Deliberately not tracked by the shutdown WaitGroup: nothing should wait
	// minutes for this timer when the bot is stopping.
	go func() {
		timer := time.NewTimer(lifetime)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		r.bot.sessions.remove(token)
		// If the requester already closed it, this edit is a no-op.
		if err := r.edit(ctx, msgID, redactedMessage, clearKeyboard()); err != nil && !isNotModified(err) {
			r.bot.log.Warn("Failed to auto-redact search results", "error", err)
		}
	}()
}

func (r *request) runSearch(ctx context.Context, query, itemType string) ([]*drive.SearchItem, error) {
	cloner, err := drive.New(ctx, r.bot.cfg, "")
	if err != nil {
		return nil, err
	}
	return cloner.Search(query, searchLimit, itemType)
}

// parseSearchArgs splits the query from a trailing --dir/--all flag.
func parseSearchArgs(text string) (query, itemType, parseErr string) {
	args := searchTerms(text)

	dirCount := countArg(args, "--dir")
	allCount := countArg(args, "--all")
	if dirCount > 1 || allCount > 1 {
		return "", "files", "Use --dir/--all only once."
	}
	if dirCount > 0 && allCount > 0 {
		return "", "files", "Use either --dir or --all, not both."
	}

	itemType = "files"
	if dirCount > 0 || allCount > 0 {
		flag := "--all"
		if dirCount > 0 {
			flag = "--dir"
		}
		if len(args) == 0 || args[len(args)-1] != flag {
			return "", "files", fmt.Sprintf("Use %s only at the end. Example: /s ubuntu %s", flag, flag)
		}
		if flag == "--dir" {
			itemType = "folders"
		} else {
			itemType = "all"
		}
		args = args[:len(args)-1]
	}

	return strings.TrimSpace(strings.Join(args, " ")), itemType, ""
}

func countArg(args []string, want string) int {
	count := 0
	for _, arg := range args {
		if arg == want {
			count++
		}
	}
	return count
}

// searchTerms splits a query into words. A search query is prose, not a shell
// command: "Marvel's Spider-Man 2" has to work, so an apostrophe is just a
// character and nothing here can fail. Surrounding quotes are trimmed so a
// user who types "spider man" out of habit still matches, while an internal
// apostrophe is preserved because Drive matches names literally.
func searchTerms(text string) []string {
	var terms []string
	for _, field := range strings.Fields(text) {
		field = strings.Trim(field, `'"`)
		if field == "" {
			continue
		}
		terms = append(terms, field)
	}
	return terms
}

// buildSearchPageText renders one page of results, clamping the page number.
func buildSearchPageText(results []*drive.SearchItem, query string, page int) (string, int, int) {
	totalCount := len(results)
	totalPages := (totalCount + searchResultsPerPage - 1) / searchResultsPerPage
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 0 {
		page = 0
	}
	if page > totalPages-1 {
		page = totalPages - 1
	}

	start := page * searchResultsPerPage
	end := start + searchResultsPerPage
	if end > totalCount {
		end = totalCount
	}

	lines := []string{
		header("SEARCH RESULTS"),
		codeBlock(query),
		fmt.Sprintf("<b>Page %d/%d | Total: %d</b>\n", page+1, totalPages, totalCount),
	}
	for _, item := range results[start:end] {
		entry := codeBlock(item.Name()) + " • " + html.EscapeString(searchItemSizeText(item)) + "\n"
		if token := encodeDriveIDToken(item.ID()); token != "" {
			entry += "ID: " + codeBlock(token) + "\n"
		}
		lines = append(lines, entry)
	}

	return strings.Join(lines, "\n"), totalPages, page
}

func searchItemSizeText(item *drive.SearchItem) string {
	if size, ok := item.Size(); ok {
		return progress.FormatBytes(float64(size))
	}
	return "Unknown"
}

func buildSearchPageMarkup(token string, page, totalPages int) tg.ReplyMarkupClass {
	var nav []tg.KeyboardButtonClass
	if page > 0 {
		nav = append(nav, markup.Callback("PREV", []byte("gds:"+token+":"+strconv.Itoa(page-1))))
	}
	nav = append(nav, markup.Callback(
		fmt.Sprintf("%d/%d", page+1, totalPages),
		[]byte("gdsnoop"),
	))
	if page < totalPages-1 {
		nav = append(nav, markup.Callback("NEXT", []byte("gds:"+token+":"+strconv.Itoa(page+1))))
	}

	return markup.InlineKeyboard(
		tg.KeyboardButtonRow{Buttons: nav},
		tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			markup.Callback("CLOSE", []byte("gdsc:"+token)),
		}},
	)
}

func (b *Bot) onCallbackQuery(e tg.Entities, u *tg.UpdateBotCallbackQuery) error {
	data := string(u.Data)
	switch {
	case data == "gdsnoop":
		b.goHandle("callback", func(ctx context.Context) {
			b.answerCallback(ctx, u.QueryID, "", false)
		})
	case strings.HasPrefix(data, "gdsc:"):
		b.goHandle("callback-close", func(ctx context.Context) {
			b.closeSearch(ctx, e, u, strings.TrimPrefix(data, "gdsc:"))
		})
	case strings.HasPrefix(data, "gds:"):
		b.goHandle("callback-page", func(ctx context.Context) {
			b.paginateSearch(ctx, e, u, data)
		})
	}
	return nil
}

func (b *Bot) closeSearch(ctx context.Context, e tg.Entities, u *tg.UpdateBotCallbackQuery, token string) {
	session, ok := b.sessions.get(token)
	if !ok {
		b.answerCallback(ctx, u.QueryID, "Session expired", true)
		return
	}
	if u.UserID != session.userID {
		b.answerCallback(ctx, u.QueryID, "Only requester can close this.", true)
		return
	}

	b.sessions.remove(token)
	if err := b.editCallbackMessage(ctx, e, u, redactedMessage, clearKeyboard()); err != nil && !isNotModified(err) {
		b.log.Warn("Failed to redact search results", "error", err)
	}
	b.answerCallback(ctx, u.QueryID, "Closed", false)
}

func (b *Bot) paginateSearch(ctx context.Context, e tg.Entities, u *tg.UpdateBotCallbackQuery, data string) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 {
		b.answerCallback(ctx, u.QueryID, "", false)
		return
	}

	token, pageRaw := parts[1], parts[2]
	session, ok := b.sessions.get(token)
	if !ok {
		b.answerCallback(ctx, u.QueryID, "Session expired", true)
		return
	}
	if u.UserID != session.userID {
		b.answerCallback(ctx, u.QueryID, "Only requester can change pages.", true)
		return
	}

	page, err := strconv.Atoi(pageRaw)
	if err != nil {
		b.answerCallback(ctx, u.QueryID, "", false)
		return
	}

	text, totalPages, safePage := buildSearchPageText(session.results, session.query, page)
	if err := b.editCallbackMessage(ctx, e, u, text, buildSearchPageMarkup(token, safePage, totalPages)); err != nil {
		if !isNotModified(err) {
			b.log.Warn("Failed to edit search page", "error", err)
		}
	}
	b.answerCallback(ctx, u.QueryID, "", false)
}

func (b *Bot) editCallbackMessage(
	ctx context.Context,
	e tg.Entities,
	u *tg.UpdateBotCallbackQuery,
	text string,
	replyMarkup tg.ReplyMarkupClass,
) error {
	inputPeer, err := messagepeer.EntitiesFromUpdate(e).ExtractPeer(u.Peer)
	if err != nil {
		return err
	}

	builder := b.sender.To(inputPeer).NoWebpage()
	if replyMarkup != nil {
		builder = builder.Markup(replyMarkup)
	}
	_, err = builder.Edit(u.MsgID).StyledText(ctx, tdhtml.String(nil, text))
	return err
}

func (b *Bot) answerCallback(ctx context.Context, queryID int64, text string, alert bool) {
	req := &tg.MessagesSetBotCallbackAnswerRequest{QueryID: queryID}
	if text != "" {
		req.SetMessage(text)
	}
	if alert {
		req.SetAlert(true)
	}
	if _, err := b.api.MessagesSetBotCallbackAnswer(ctx, req); err != nil {
		b.log.Warn("Failed to answer callback query", "error", err)
	}
}
