package bot

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gotd/td/telegram/message"
	messagepeer "github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

// LogFilePath is the rotating log the /logs command uploads.
const LogFilePath = "logs/clonebot.log"

// handleLogs sends the current log file to the owner.
func (r *request) handleLogs(ctx context.Context) {
	if r.rejectNonOwner(ctx) {
		return
	}

	log := r.bot.log.With("context", r.context())
	log.Info("Logs requested")

	info, err := os.Stat(LogFilePath)
	if err != nil || info.Size() == 0 {
		r.replyLogged(ctx, "No logs yet")
		return
	}

	file, err := uploader.NewUploader(r.bot.api).FromPath(ctx, LogFilePath)
	if err != nil {
		log.Warn("Failed to upload log file", "error", err)
		r.replyLogged(ctx, "Error sending log file")
		return
	}

	_, err = r.bot.sender.Answer(r.entities, r.update).
		ReplyMsg(r.msg).
		Media(ctx, message.UploadedDocument(file).
			Filename(filepath.Base(LogFilePath)).
			MIME("text/plain").
			ForceFile(true))
	if err != nil {
		log.Warn("Failed to send log file", "error", err)
		r.replyLogged(ctx, "Error sending log file")
		return
	}
	log.Info("Logs sent", "bytes", info.Size())
}

// authTarget resolves who an /auth or /unauth applies to: the user whose
// message was replied to, an explicit ID, or the current chat.
func (r *request) authTarget(ctx context.Context) (id int64, isUser, ok bool) {
	if userID := r.repliedUserID(ctx); userID != 0 {
		return userID, true, true
	}
	if len(r.args) > 0 {
		id, err := strconv.ParseInt(r.args[0], 10, 64)
		if err != nil {
			return 0, false, false
		}
		return id, false, true
	}
	return r.chatID(), false, true
}

// repliedUserID returns the sender of the message this command replied to, or
// 0. MTProto does not carry the original sender in the reply header, so the
// replied-to message has to be fetched.
func (r *request) repliedUserID(ctx context.Context) int64 {
	header, ok := r.msg.ReplyTo.(*tg.MessageReplyHeader)
	if !ok || header.ReplyToMsgID == 0 {
		return 0
	}

	inputPeer, err := messagepeer.EntitiesFromUpdate(r.entities).ExtractPeer(r.msg.PeerID)
	if err != nil {
		return 0
	}
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: header.ReplyToMsgID}}

	var messages tg.MessagesMessagesClass
	if channel, ok := inputPeer.(*tg.InputPeerChannel); ok {
		messages, err = r.bot.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
			ID:      ids,
		})
	} else {
		messages, err = r.bot.api.MessagesGetMessages(ctx, ids)
	}
	if err != nil {
		r.bot.log.Warn("Failed to fetch replied message", "error", err)
		return 0
	}

	replied, ok := messages.AsModified()
	if !ok {
		return 0
	}
	for _, m := range replied.GetMessages() {
		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		if from, ok := msg.FromID.(*tg.PeerUser); ok {
			return from.UserID
		}
		// A message in a private chat carries no FromID.
		if peer, ok := msg.PeerID.(*tg.PeerUser); ok {
			return peer.UserID
		}
	}
	return 0
}

// userSuffix labels replies whose target came from a replied-to message, so it
// is obvious a user was authorized rather than a chat.
func userSuffix(isUser bool) string {
	if isUser {
		return " [User]"
	}
	return ""
}

func (r *request) handleAuth(ctx context.Context) {
	if r.rejectNonOwner(ctx) {
		return
	}

	id, isUser, ok := r.authTarget(ctx)
	if !ok {
		r.replyLogged(ctx, "Please provide a valid numeric ID.")
		return
	}

	if id == r.bot.cfg.OwnerID || r.bot.cfg.IsAuthorizedChat(id) {
		r.replyLogged(ctx, "Already authorized!"+userSuffix(isUser))
		return
	}

	added, err := r.bot.auth.Authorize(id, r.userID())
	if err != nil {
		r.bot.log.Error("Failed to authorize", "id", id, "error", err)
		r.replyLogged(ctx, "Error saving authorization")
		return
	}
	if !added {
		r.replyLogged(ctx, "Already authorized!"+userSuffix(isUser))
		return
	}

	r.bot.log.Info("Authorized", "id", id, "by", r.userID())
	r.replyLogged(ctx, "Authorized "+codeBlock(itoa(id))+userSuffix(isUser))
}

func (r *request) handleUnauth(ctx context.Context) {
	if r.rejectNonOwner(ctx) {
		return
	}

	id, isUser, ok := r.authTarget(ctx)
	if !ok {
		r.replyLogged(ctx, "Please provide a valid numeric ID.")
		return
	}

	if id == r.bot.cfg.OwnerID {
		r.replyLogged(ctx, "Bot admin!"+userSuffix(isUser))
		return
	}
	// An .env entry would come back on the next start, so say where it lives.
	if r.bot.cfg.IsAuthorizedChat(id) {
		r.replyLogged(ctx, "Authorized in .env, remove it there"+userSuffix(isUser))
		return
	}

	removed, err := r.bot.auth.Revoke(id)
	if err != nil {
		r.bot.log.Error("Failed to revoke authorization", "id", id, "error", err)
		r.replyLogged(ctx, "Error saving authorization")
		return
	}
	if !removed {
		r.replyLogged(ctx, "Already unauthorized!"+userSuffix(isUser))
		return
	}

	r.bot.log.Info("Revoked authorization", "id", id, "by", r.userID())
	r.replyLogged(ctx, "Unauthorized "+codeBlock(itoa(id))+userSuffix(isUser))
}
