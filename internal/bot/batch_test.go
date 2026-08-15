package bot

import (
	"strings"
	"testing"
)

func newTestBatch(count int) *batch {
	b := &batch{}
	for i := 0; i < count; i++ {
		name := string(rune('a' + i))
		b.items = append(b.items, &batchItem{source: name, name: name, text: name})
	}
	return b
}

func TestBatchTextShowsShortQueueWhole(t *testing.T) {
	b := newTestBatch(3)
	if got := b.text(); got != "a\n\nb\n\nc" {
		t.Errorf("text() = %q, want the blocks joined unchanged", got)
	}
}

func TestBatchTextCountsHiddenBlocks(t *testing.T) {
	b := newTestBatch(10)

	// Nothing started yet: the first page of the queue, rest counted below.
	got := b.text()
	if !strings.HasPrefix(got, "a\n\nb") || !strings.HasSuffix(got, "<b>+4 more</b>") {
		t.Errorf("text() = %q, want the first %d blocks and a remaining count", got, maxStatusBlocks)
	}
	if strings.Contains(got, "above") {
		t.Errorf("text() = %q, want no dropped count before the first item", got)
	}

	// Working on the eighth item: the window slid to end on it.
	b.active = 7
	got = b.text()
	if !strings.HasPrefix(got, "<b>+2 above</b>") || !strings.HasSuffix(got, "<b>+2 more</b>") {
		t.Errorf("text() = %q, want counts on both sides of the window", got)
	}
	if !strings.Contains(got, "\nh") {
		t.Errorf("text() = %q, want the active block visible", got)
	}
}

func TestBatchTextWindowStopsAtTheEnd(t *testing.T) {
	b := newTestBatch(8)
	b.active = 7

	got := b.text()
	if !strings.HasPrefix(got, "<b>+2 above</b>") {
		t.Errorf("text() = %q, want the two finished blocks counted", got)
	}
	if strings.Contains(got, "more</b>") {
		t.Errorf("text() = %q, want no remaining count on the last item", got)
	}
	if blocks := strings.Count(got, "\n\n"); blocks != maxStatusBlocks {
		t.Errorf("text() = %q, want %d blocks after the dropped count", got, maxStatusBlocks)
	}
}
