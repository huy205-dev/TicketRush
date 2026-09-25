// Command notifier sends a message when tickets are issued or a payment is
// refunded: to Telegram when TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID are
// set, otherwise to the log.
package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/notifier"
	"github.com/huy205-dev/ticketrush/internal/platform/app"
	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
)

const (
	serviceName     = "notifier"
	consumerGroup   = "notifier"
	telegramTimeout = 10 * time.Second
)

func main() {
	ctx, stop := app.SignalContext()
	err := run(ctx, os.LookupEnv, os.Stdout)
	stop()
	if err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, lookupEnv func(string) (string, bool), stdout io.Writer) error {
	cfg, logger, err := app.Setup(serviceName, lookupEnv, stdout)
	if err != nil {
		return err
	}
	rdb := redisx.NewClient(cfg.RedisAddr)
	defer func() { _ = rdb.Close() }()

	var sender notifier.Sender = notifier.LogSender{Logger: logger}
	if cfg.TelegramBotToken != "" {
		sender = notifier.NewTelegram(notifier.TelegramAPI, cfg.TelegramBotToken, cfg.TelegramChatID, telegramTimeout)
	}
	logger.Info("notifier configured", "telegram", cfg.TelegramBotToken != "")

	n := notifier.New(sender, notifier.NewRedisSeen(rdb), logger)
	topics := []string{events.TopicOrders, events.TopicTickets}
	err = kafka.Run(ctx, logger, cfg.KafkaBrokers, consumerGroup, topics, n.Handler())
	if err != nil {
		logger.Error("consumer stopped with error", "err", err)
	}
	return err
}
