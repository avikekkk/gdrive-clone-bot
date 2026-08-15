package progress

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestFormatBytes(t *testing.T) {
	cases := map[float64]string{
		0:                    "0 B",
		512:                  "512 B",
		1024:                 "1.0 KB",
		1536:                 "1.5 KB",
		1024 * 1024:          "1.0 MB",
		1024 * 1024 * 10:     "10.0 MB",
		1024 * 1024 * 1024:   "1.0 GB",
		1288490188:           "1.2 GB",
		1342177280:           "1.25 GB",
		1024 * 1024 * 1024.5: "1.0 GB",
		-5:                   "0 B",
	}
	for input, want := range cases {
		if got := FormatBytes(input); got != want {
			t.Errorf("FormatBytes(%v) = %q, want %q", input, got, want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[float64]string{
		0:           "00:00",
		59:          "00:59",
		61:          "01:01",
		3661:        "01:01:01",
		-1:          "--",
		math.NaN():  "--",
		math.Inf(1): "--",
	}
	for input, want := range cases {
		if got := FormatDuration(input); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", input, got, want)
		}
	}
}

func TestClonePercentageAndBar(t *testing.T) {
	c := NewClone("movie.mkv")
	c.TotalBytes = 1000
	c.CopiedBytes = 250

	if got := c.Percentage(); got != 25 {
		t.Errorf("Percentage() = %v, want 25", got)
	}
	bar := c.Bar(12)
	if strings.Count(bar, "▓") != 3 || strings.Count(bar, "░") != 9 {
		t.Errorf("Bar(12) = %q, want 3 filled of 12", bar)
	}

	// With no byte total, progress falls back to file counts.
	c.TotalBytes = 0
	c.TotalFiles = 4
	c.CopiedFiles = 1
	if got := c.Percentage(); got != 25 {
		t.Errorf("file-based Percentage() = %v, want 25", got)
	}
}

func TestCloneETAUnknownWithoutTotal(t *testing.T) {
	c := NewClone("folder")
	if !math.IsNaN(c.ETASeconds()) {
		t.Error("ETA should be unknown without a byte total")
	}
	if got := FormatDuration(c.ETASeconds()); got != "--" {
		t.Errorf("FormatDuration(NaN) = %q, want --", got)
	}
}

func TestCloneMessageEscapesName(t *testing.T) {
	c := NewClone("a <i> & tag")
	msg := c.Message()
	if strings.Contains(msg, "<i>") || !strings.Contains(msg, "a &lt;i&gt; &amp; tag") {
		t.Errorf("Message() did not escape the task name: %q", msg)
	}
}

func TestThrottle(t *testing.T) {
	throttle := NewThrottle(time.Hour)
	if !throttle.ShouldUpdate(false) {
		t.Error("first update should be allowed")
	}
	if throttle.ShouldUpdate(false) {
		t.Error("second immediate update should be throttled")
	}
	if !throttle.ShouldUpdate(true) {
		t.Error("forced update should bypass throttling")
	}
}
