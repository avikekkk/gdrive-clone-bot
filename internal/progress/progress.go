// Package progress renders live clone progress as Telegram HTML messages.
package progress

import (
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
	"time"
)

// FormatBytes renders a byte count using binary units, rounded to two decimals
// with trailing zeros trimmed: "1.5 KB", "1.0 MB", "1.25 GB".
func FormatBytes(numBytes float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := math.Max(numBytes, 0)
	for i, unit := range units {
		if value < 1024 || i == len(units)-1 {
			if unit == "B" {
				return fmt.Sprintf("%d %s", int64(value), unit)
			}
			return trimZeros(value) + " " + unit
		}
		value /= 1024
	}
	return trimZeros(value) + " TB"
}

// trimZeros rounds to two decimals and drops trailing zeros, keeping at least
// one decimal place so sizes still read as measurements.
func trimZeros(value float64) string {
	rounded := math.Round(value*100) / 100
	text := strconv.FormatFloat(rounded, 'f', -1, 64)
	if !strings.Contains(text, ".") {
		text += ".0"
	}
	return text
}

// FormatDuration renders seconds as HH:MM:SS (or MM:SS under an hour).
func FormatDuration(seconds float64) string {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return "--"
	}
	total := int64(seconds)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func clip(text string, maxLen int) string {
	text = strings.ReplaceAll(strings.TrimSpace(text), "\n", " ")
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen-3]) + "..."
}

// Clone tracks the state of an in-flight clone. It is owned by a single
// goroutine: the one running the clone.
type Clone struct {
	TaskName    string
	TotalBytes  int64
	TotalFiles  int64
	CopiedBytes int64
	CopiedFiles int64
	StartedAt   time.Time
	Status      string
}

// NewClone creates a Clone for the named task.
func NewClone(taskName string) *Clone {
	return &Clone{
		TaskName:  taskName,
		StartedAt: time.Now(),
		Status:    "Initializing",
	}
}

// Percentage is byte-based when a total size is known, else file-count based.
func (c *Clone) Percentage() float64 {
	if c.TotalBytes > 0 {
		return math.Min(100, float64(c.CopiedBytes)/float64(c.TotalBytes)*100)
	}
	if c.TotalFiles > 0 {
		return math.Min(100, float64(c.CopiedFiles)/float64(c.TotalFiles)*100)
	}
	return 0
}

// Elapsed is clamped away from zero so it is always safe to divide by.
func (c *Clone) Elapsed() float64 {
	return math.Max(0.001, time.Since(c.StartedAt).Seconds())
}

// SpeedBPS is the average copy speed so far, in bytes per second.
func (c *Clone) SpeedBPS() float64 {
	if c.CopiedBytes <= 0 {
		return 0
	}
	return float64(c.CopiedBytes) / c.Elapsed()
}

// ETASeconds returns NaN when no meaningful estimate exists yet.
func (c *Clone) ETASeconds() float64 {
	if c.TotalBytes <= 0 {
		return math.NaN()
	}
	remaining := c.TotalBytes - c.CopiedBytes
	if remaining < 0 {
		remaining = 0
	}
	speed := c.SpeedBPS()
	if speed <= 0 {
		return math.NaN()
	}
	return float64(remaining) / speed
}

// Bar renders a fixed-width unicode progress bar.
func (c *Clone) Bar(width int) string {
	pct := math.Max(0, math.Min(100, c.Percentage()))
	filled := int(pct / 100 * float64(width))
	return "[" + strings.Repeat("▓", filled) + strings.Repeat("░", width-filled) + "]"
}

// Message renders the live progress message.
func (c *Clone) Message() string {
	totalSize := "Unknown"
	if c.TotalBytes != 0 {
		totalSize = FormatBytes(float64(c.TotalBytes))
	}

	return "<u><b>CLONING</b></u>\n" +
		fmt.Sprintf("<code>%s</code>\n", html.EscapeString(clip(c.TaskName, 90))) +
		fmt.Sprintf("<code>%s %.2f%%</code>\n", html.EscapeString(c.Bar(12)), c.Percentage()) +
		fmt.Sprintf("<code>%s</code> <code>/</code> <code>%s</code>\n",
			html.EscapeString(FormatBytes(float64(c.CopiedBytes))), html.EscapeString(totalSize)) +
		fmt.Sprintf("<code>↓%s | ETA : %s</code>",
			html.EscapeString(FormatBytes(c.SpeedBPS())+"/s"), html.EscapeString(FormatDuration(c.ETASeconds())))
}

// CompletionMessage renders the final message for a finished clone.
func (c *Clone) CompletionMessage(outputName, outputURL string) string {
	return "<u><b>CLONED</b></u>\n" +
		fmt.Sprintf("<code>%s</code>\n", html.EscapeString(outputName)) +
		fmt.Sprintf("<b>%s •</b> ", html.EscapeString(FormatBytes(float64(c.CopiedBytes)))) +
		fmt.Sprintf("<a href=\"%s\"><b>DL</b></a>", html.EscapeString(outputURL))
}

// Throttle rate-limits progress edits so Telegram does not throttle us instead.
type Throttle struct {
	MinInterval  time.Duration
	lastUpdateAt time.Time
}

// NewThrottle creates a Throttle with the given minimum edit interval.
func NewThrottle(minInterval time.Duration) *Throttle {
	return &Throttle{MinInterval: minInterval}
}

// ShouldUpdate reports whether enough time has passed since the last edit.
func (t *Throttle) ShouldUpdate(force bool) bool {
	now := time.Now()
	if force || now.Sub(t.lastUpdateAt) >= t.MinInterval {
		t.lastUpdateAt = now
		return true
	}
	return false
}
