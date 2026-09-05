package bot

import (
	"context"
	"strings"

	"github.com/avisek/gdrive-clone-bot/internal/progress"
)

// batchItem is one source and the block of the status message showing it.
type batchItem struct {
	source string
	name   string
	text   string
}

// batch tracks every source of one /c or /n command inside a single status
// message. Each item owns a block of that message and blocks are re-rendered in
// place as items move from QUEUED to running to their final state, so a command
// carrying ten IDs still costs the chat exactly one message.
type batch struct {
	r        *request
	msgID    int
	items    []*batchItem
	throttle *progress.Throttle

	// active is the item being worked on, which the displayed window follows.
	active int
}

func (b *batch) multi() bool { return len(b.items) > 1 }

// startBatch posts the status message and seeds one block per source. A batch
// goes up as the queue itself, listing raw IDs until each name resolves; a lone
// source shows Checking, which is all its first block would say anyway.
func (r *request) startBatch(ctx context.Context, sources []string) *batch {
	b := &batch{r: r, throttle: progress.NewThrottle(progressEditInterval)}
	for _, source := range sources {
		item := &batchItem{source: source, name: source, text: statusChecking}
		if len(sources) > 1 {
			item.text = queuedBlock(source)
		}
		b.items = append(b.items, item)
	}

	msgID, err := r.reply(ctx, b.text())
	if err != nil {
		r.bot.log.Warn("Failed to post status message", "err", err)
		return nil
	}
	b.msgID = msgID
	if !b.multi() {
		return b
	}

	// Names need a Drive lookup each, so they fill in behind the queue rather
	// than holding it back.
	for index, item := range b.items {
		item.name = r.sourceName(ctx, item.source)
		b.set(ctx, index, queuedBlock(item.name), index == len(b.items)-1)
	}
	return b
}

func queuedBlock(name string) string { return header("QUEUED") + "\n" + codeBlock(name) }

// set replaces one item's block and redraws the message.
func (b *batch) set(ctx context.Context, index int, text string, force bool) {
	b.items[index].text = text
	b.render(ctx, force)
}

// status shows a transient status such as Checking or Nuking. In a batch it
// becomes an upper-case header above the item name, so the block says which
// source it belongs to; on its own it is just the status.
func (b *batch) status(ctx context.Context, index int, status string) {
	// Every item opens with a status, so this is where the window advances.
	b.active = index
	if !b.multi() {
		b.set(ctx, index, status, true)
		return
	}
	b.set(ctx, index, header(strings.ToUpper(status))+"\n"+codeBlock(b.items[index].name), true)
}

// fail shows a final error block, keeping the item name visible in a batch.
func (b *batch) fail(ctx context.Context, index int, text string) {
	if b.multi() {
		text = codeBlock(b.items[index].name) + "\n" + text
	}
	b.set(ctx, index, text, true)
}

// render rewrites the status message from the current blocks. Unforced redraws
// are throttled, since progress updates arrive far faster than Telegram allows
// edits.
func (b *batch) render(ctx context.Context, force bool) {
	if !b.throttle.ShouldUpdate(force) {
		return
	}
	b.r.editLogged(ctx, b.msgID, b.text())
}

// maxStatusBlocks caps how many item blocks the status message shows at once,
// the way nzbget's /status shows six tasks and counts the rest.
const maxStatusBlocks = 6

// text renders the status message: a window of at most maxStatusBlocks blocks
// ending at the item being worked on, with counts standing in for whatever sits
// above and below it.
func (b *batch) text() string {
	start := 0
	if len(b.items) > maxStatusBlocks {
		start = min(max(b.active-maxStatusBlocks+1, 0), len(b.items)-maxStatusBlocks)
	}
	end := min(start+maxStatusBlocks, len(b.items))

	blocks := make([]string, 0, end-start)
	for _, item := range b.items[start:end] {
		blocks = append(blocks, item.text)
	}
	text := strings.Join(blocks, "\n\n")

	if start > 0 {
		text = "<b>+" + itoa(int64(start)) + " above</b>\n\n" + text
	}
	if remaining := len(b.items) - end; remaining > 0 {
		text += "\n\n<b>+" + itoa(int64(remaining)) + " more</b>"
	}
	return text
}

func header(text string) string { return "<u><b>" + text + "</b></u>" }
