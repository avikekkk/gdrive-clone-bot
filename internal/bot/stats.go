package bot

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/avisek/gdrive-clone-bot/internal/progress"
)

// readableTime renders a duration compactly, as "1d2h3m4s".
func readableTime(d time.Duration) string {
	seconds := int64(d.Seconds())
	if seconds <= 0 {
		return "0s"
	}

	periods := []struct {
		name    string
		seconds int64
	}{
		{"d", 86400},
		{"h", 3600},
		{"m", 60},
		{"s", 1},
	}

	var result strings.Builder
	for _, period := range periods {
		if seconds >= period.seconds {
			value := seconds / period.seconds
			seconds -= value * period.seconds
			fmt.Fprintf(&result, "%d%s", value, period.name)
		}
	}
	return result.String()
}

func statLine(label, value string) string {
	return "<code>" + label + ":</code> <code>" + html.EscapeString(value) + "</code>"
}

// handleServer reports stats for the host the bot runs on.
func (r *request) handleServer(ctx context.Context) {
	if r.rejectUnauthorizedClone(ctx) {
		return
	}

	log := r.bot.log.With("context", r.context())
	log.Info("Server stats requested")

	statsMsgID, err := r.reply(ctx, "Fetching stats")
	if err != nil {
		log.Warn("Failed to post status message", "error", err)
		return
	}

	r.editLogged(ctx, statsMsgID, r.systemStats())
}

// systemStats describes the host the bot runs on.
func (r *request) systemStats() string {
	lines := []string{
		header("SYSTEM STATS"),
		statLine("Bot Uptime", readableTime(time.Since(r.bot.startedAt))),
	}

	var diskPercent float64
	if usage, err := disk.Usage("/"); err == nil {
		diskPercent = usage.UsedPercent
		lines = append(lines, "<code>Disk:</code> <code>"+
			html.EscapeString(progress.FormatBytes(float64(usage.Total)))+"</code> <code>|</code> "+
			"<code>Free:</code> <code>"+html.EscapeString(progress.FormatBytes(float64(usage.Free)))+"</code>")
	}

	var cpuPercent float64
	if percents, err := cpu.Percent(500*time.Millisecond, false); err == nil && len(percents) > 0 {
		cpuPercent = percents[0]
	}
	var ramPercent float64
	if memory, err := mem.VirtualMemory(); err == nil {
		ramPercent = memory.UsedPercent
	}

	lines = append(lines, fmt.Sprintf(
		"\n<code>CPU:</code> <code>%.1f%%</code> <code>|</code> <code>RAM:</code> <code>%.1f%%</code> "+
			"<code>|</code> <code>DISK:</code> <code>%.1f%%</code>",
		cpuPercent, ramPercent, diskPercent,
	))

	return strings.Join(lines, "\n")
}
