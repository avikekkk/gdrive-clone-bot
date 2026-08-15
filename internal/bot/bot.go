// Package bot implements the Telegram front end: command routing,
// authorization, and live progress reporting.
package bot

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/avisek/gdrive-clone-bot/internal/config"
	"github.com/avisek/gdrive-clone-bot/internal/store"
)

const helpText = "<u><b>BOT COMMANDS</b></u>\n\n" +
	"<b>✦ To search Shared Drives:</b>\n\n" +
	"<code>/s [query]</code>\n\n" +
	"<b>✦ Type flags:</b>\n\n" +
	"The flags <code>--dir</code> and <code>--all</code> are optional and must be the last word of a <code>/s</code>.\n\n" +
	"  <code>--dir</code>    [Folders only]\n" +
	"  <code>--all</code>    [Files and folders]\n\n" +
	"Without a flag, results come back files only, largest first.\n\n" +
	"<b>✦ To clone from search results:</b>\n\n" +
	"<code>/c [ID-1] [ID-2]...</code>\n\n" +
	"These IDs are the Drive IDs shown under each search result, separated by spaces, commas or new lines. " +
	"Drive links and raw file IDs work too.\n\n" +
	"<b>✦ To delete a file or folder:</b>\n\n" +
	"<code>/n [ID-1] [ID-2]...</code>\n\n" +
	"Takes the same links, IDs, and search result IDs as <code>/c</code>.\n\n" +
	"Several IDs are queued in one status message and processed one by one.\n\n" +
	"<b>✦ To view the status:</b>\n\n" +
	"<code>/server</code>    [system stats]\n\n" +
	"<b>✦ Admin only:</b>\n\n" +
	"  <code>/logs</code>            [bot log file]\n" +
	"  <code>/auth [id]</code>       [authorize a user or chat]\n" +
	"  <code>/unauth [id]</code>     [remove authorization]\n" +
	"  <code>/restart</code>         [restart the bot]"

// Bot holds everything the update handlers need.
type Bot struct {
	cfg    *config.Bot
	log    *slog.Logger
	sender *message.Sender
	api    *tg.Client

	// runCtx outlives a single update, so long clones are not cancelled when
	// update processing returns.
	runCtx context.Context

	sessions  *sessionStore
	auth      *store.Store
	startedAt time.Time
	wg        sync.WaitGroup
}

// Run connects to Telegram as a bot and serves updates until ctx is cancelled.
func Run(ctx context.Context, cfg *config.Bot, logger *slog.Logger) error {
	auth, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer auth.Close()
	logger.Info("Authorizations stored in " + cfg.DatabasePath)

	dispatcher := tg.NewUpdateDispatcher()
	b := &Bot{
		cfg:       cfg,
		log:       logger,
		sessions:  newSessionStore(maxSearchSessions),
		auth:      auth,
		startedAt: time.Now(),
	}

	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		return b.onMessage(e, u)
	})
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		return b.onMessage(e, u)
	})
	dispatcher.OnBotCallbackQuery(func(ctx context.Context, e tg.Entities, u *tg.UpdateBotCallbackQuery) error {
		return b.onCallbackQuery(e, u)
	})

	client := telegram.NewClient(cfg.TelegramAPIID, cfg.TelegramAPIHash, telegram.Options{
		UpdateHandler: dispatcher,
		// Bots re-authenticate from their token on every start, so an
		// in-memory session is enough and leaves no session file behind.
		SessionStorage: &session.StorageMemory{},
	})

	return client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !status.Authorized {
			if _, err := client.Auth().Bot(ctx, cfg.TelegramBotToken); err != nil {
				return err
			}
		}

		b.runCtx = ctx
		b.api = tg.NewClient(client)
		b.sender = message.NewSender(b.api)

		self, err := client.Self(ctx)
		if err != nil {
			return err
		}
		logger.Info("Bot started", "username", self.Username, "user_id", self.ID)

		<-ctx.Done()
		// Give in-flight commands a moment to report their final state.
		b.wg.Wait()
		return ctx.Err()
	})
}

// go runs a command handler off the update loop so a long clone does not block
// further updates.
func (b *Bot) goHandle(name string, fn func(ctx context.Context)) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				b.log.Error("Handler panicked", "handler", name, "panic", r)
			}
		}()
		fn(b.runCtx)
	}()
}

// messageUpdate is satisfied by both private/basic-group and channel updates.
type messageUpdate interface {
	message.AnswerableMessageUpdate
}

func (b *Bot) onMessage(e tg.Entities, u messageUpdate) error {
	msg, ok := u.GetMessage().(*tg.Message)
	if !ok || msg.Out {
		return nil
	}

	command, args, payload, ok := parseCommand(msg.Message)
	if !ok {
		return nil
	}

	req := &request{bot: b, entities: e, update: u, msg: msg, args: args, payload: payload}

	switch command {
	case "start", "help":
		b.goHandle(command, req.handleHelp)
	case "c":
		b.goHandle("clone", req.handleClone)
	case "s":
		b.goHandle("search", req.handleSearch)
	case "n":
		b.goHandle("delete", req.handleDelete)
	case "logs":
		b.goHandle("logs", req.handleLogs)
	case "auth":
		b.goHandle("auth", req.handleAuth)
	case "unauth":
		b.goHandle("unauth", req.handleUnauth)
	case "server":
		b.goHandle("server", req.handleServer)
	case "restart":
		b.goHandle("restart", req.handleRestart)
	}
	return nil
}

// parseCommand splits "/cmd@bot arg1 arg2" into its parts.
func parseCommand(text string) (command string, args []string, payload string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", nil, "", false
	}

	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", nil, "", false
	}

	command = strings.TrimPrefix(fields[0], "/")
	if at := strings.Index(command, "@"); at >= 0 {
		command = command[:at]
	}
	command = strings.ToLower(command)
	if command == "" {
		return "", nil, "", false
	}

	args = fields[1:]
	if parts := strings.SplitN(text, " ", 2); len(parts) == 2 {
		payload = strings.TrimSpace(parts[1])
	}
	return command, args, payload, true
}

// request bundles one command invocation with the plumbing needed to answer it.
type request struct {
	bot      *Bot
	entities tg.Entities
	update   messageUpdate
	msg      *tg.Message
	args     []string
	payload  string
}

func (r *request) userID() int64 {
	if from, ok := r.msg.FromID.(*tg.PeerUser); ok {
		return from.UserID
	}
	// In private chats the sender is implied by the peer.
	if peer, ok := r.msg.PeerID.(*tg.PeerUser); ok {
		return peer.UserID
	}
	return 0
}

// chatID is rendered in the Bot API numbering scheme so AUTHORIZED_CHAT_IDS
// values copied from other bots keep working.
func (r *request) chatID() int64 {
	switch peer := r.msg.PeerID.(type) {
	case *tg.PeerUser:
		return peer.UserID
	case *tg.PeerChat:
		return -peer.ChatID
	case *tg.PeerChannel:
		return -1000000000000 - peer.ChannelID
	}
	return 0
}

func (r *request) context() string {
	return "chat_id=" + itoa(r.chatID()) + " user_id=" + itoa(r.userID())
}

func (r *request) isOwner() bool { return r.userID() == r.bot.cfg.OwnerID }

// isAuthorizedCloneChat allows the static .env allowlist plus anything granted
// at runtime with /auth, by either chat or user ID.
func (r *request) isAuthorizedCloneChat() bool {
	if r.bot.cfg.IsAuthorizedChat(r.chatID()) {
		return true
	}
	authorized, err := r.bot.auth.IsAuthorized(r.chatID(), r.userID())
	if err != nil {
		r.bot.log.Error("Failed to check authorization", "error", err)
		return false
	}
	return authorized
}

// rejectNonOwner answers and reports true when the sender is not the owner.
func (r *request) rejectNonOwner(ctx context.Context) bool {
	if r.isOwner() {
		return false
	}
	r.bot.log.Warn("Unauthorized command rejected", "context", r.context())
	if _, err := r.reply(ctx, "<code>Unauthorized</code>"); err != nil {
		r.bot.log.Warn("Failed to send rejection", "error", err)
	}
	return true
}

// rejectUnauthorizedClone allows the owner plus any authorized chat.
func (r *request) rejectUnauthorizedClone(ctx context.Context) bool {
	if r.isOwner() || r.isAuthorizedCloneChat() {
		return false
	}
	r.bot.log.Warn("Unauthorized clone command rejected", "context", r.context())
	if _, err := r.reply(ctx, "<code>Unauthorized</code>"); err != nil {
		r.bot.log.Warn("Failed to send rejection", "error", err)
	}
	return true
}

// reply posts an HTML reply and returns the new message ID.
func (r *request) reply(ctx context.Context, text string) (int, error) {
	return unpack.MessageID(
		r.bot.sender.Answer(r.entities, r.update).
			NoWebpage().
			ReplyMsg(r.msg).
			StyledText(ctx, html.String(nil, text)),
	)
}

// edit replaces the text of a message this command previously sent.
func (r *request) edit(ctx context.Context, msgID int, text string, markup tg.ReplyMarkupClass) error {
	builder := r.bot.sender.Answer(r.entities, r.update).NoWebpage()
	if markup != nil {
		builder = builder.Markup(markup)
	}
	_, err := builder.Edit(msgID).StyledText(ctx, html.String(nil, text))
	return err
}

// sourceSeparators splits a payload on whitespace and commas, so a batch of
// search result IDs can be pasted in any of those shapes.
var sourceSeparators = regexp.MustCompile(`[\s,]+`)

// sources returns every Drive link/ID/token in the command, in order and
// without duplicates.
func (r *request) sources() []string {
	seen := map[string]bool{}
	var sources []string

	for _, field := range sourceSeparators.Split(r.payload, -1) {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if decoded := decodeDriveIDToken(field); decoded != "" {
			field = decoded
		}
		if seen[field] {
			continue
		}
		seen[field] = true
		sources = append(sources, field)
	}
	return sources
}

func (r *request) handleHelp(ctx context.Context) {
	// Authorized users get the command list too; only admin actions are gated.
	if r.rejectUnauthorizedClone(ctx) {
		return
	}
	if _, err := r.reply(ctx, helpText); err != nil {
		r.bot.log.Warn("Failed to send help", "error", err)
	}
}

// isNotModified reports the benign error Telegram returns for a no-op edit.
func isNotModified(err error) bool {
	return tgerr.Is(err, "MESSAGE_NOT_MODIFIED")
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
