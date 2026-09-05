// Command clonebot runs the Google Drive cloner Telegram bot.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"

	"github.com/avisek/gdrive-clone-bot/internal/bot"
	"github.com/avisek/gdrive-clone-bot/internal/config"
)

// logFile mirrors bot.LogFilePath, which the /logs command uploads.
const logFile = bot.LogFilePath

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	logger, closeLogs, err := setupLogging()
	if err != nil {
		return err
	}
	defer closeLogs()

	cfg, err := config.Load()
	if err != nil {
		var configErr *config.Error
		if errors.As(err, &configErr) {
			return fmt.Errorf("Configuration error: %w", err)
		}
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	absLog, _ := filepath.Abs(logFile)
	logger.Info("Logging to " + absLog)

	err = bot.Run(ctx, cfg, logger)
	logger.Info("Bot stopped")
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// setupLogging writes to the console and to a size-rotated log file.
func setupLogging() (*slog.Logger, func(), error) {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}

	rotator := &lumberjack.Logger{
		Filename:   logFile,
		MaxSize:    5, // megabytes
		MaxBackups: 5,
	}

	handler := slog.NewTextHandler(io.MultiWriter(os.Stdout, rotator), &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)

	return logger, func() { _ = rotator.Close() }, nil
}
