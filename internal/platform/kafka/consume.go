package kafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Handler processes one record. Returning an error retries the record after
// a backoff, forever, unless the error is wrapped with Permanent. Handlers
// must be idempotent: delivery is at-least-once.
type Handler func(ctx context.Context, r *kgo.Record) error

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks err as not worth retrying (a malformed record, an event
// about something that cannot exist). The record is logged and skipped.
func Permanent(err error) error { return permanentError{err} }

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool {
	var perm permanentError
	return errors.As(err, &perm)
}

// Retry backoff for failing records.
const (
	retryMin = 100 * time.Millisecond
	retryMax = 5 * time.Second
)

// NewConsumer returns a client in consumer group that reads topics from the
// oldest retained record when the group is new, commits only when told to,
// and does not rebalance while a polled batch is being processed.
func NewConsumer(brokers []string, group string, topics ...string) (*kgo.Client, error) {
	return NewClient(brokers,
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
	)
}

// Consume runs handle on every record until ctx is cancelled. Records are
// processed one at a time in poll order, so each partition keeps its order
// (all events of one order share a partition). Offsets are committed after
// the whole polled batch succeeded; if ctx is cancelled mid-batch nothing is
// committed and the batch is redelivered later.
func Consume(ctx context.Context, cl *kgo.Client, logger *slog.Logger, handle Handler) error {
	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return nil
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			logger.WarnContext(ctx, "kafka fetch error", "topic", topic, "partition", partition, "err", err)
		})

		for it := fetches.RecordIter(); !it.Done(); {
			if !process(ctx, logger, handle, it.Next()) {
				return nil // cancelled; leave the batch uncommitted
			}
		}
		if err := cl.CommitUncommittedOffsets(ctx); err != nil && ctx.Err() == nil {
			logger.WarnContext(ctx, "commit offsets failed; the batch may be redelivered", "err", err)
		}
		cl.AllowRebalance()
	}
}

// process retries handle until it succeeds, fails permanently, or ctx ends.
// It returns false only when ctx ended first.
func process(ctx context.Context, logger *slog.Logger, handle Handler, r *kgo.Record) bool {
	wait := retryMin
	for {
		err := handle(ctx, r)
		if err == nil {
			return true
		}
		attrs := []any{"topic", r.Topic, "partition", r.Partition, "offset", r.Offset, "key", string(r.Key), "err", err}
		if IsPermanent(err) {
			logger.ErrorContext(ctx, "skipping record that cannot be processed", attrs...)
			return true
		}
		logger.WarnContext(ctx, fmt.Sprintf("record failed; retrying in %v", wait), attrs...)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
		wait = min(wait*2, retryMax)
	}
}

// Header returns the value of a record header, or "".
func Header(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

// Run consumes topics as group with handle until ctx is cancelled. Topics
// that do not exist yet are picked up once the relay has created them.
func Run(ctx context.Context, logger *slog.Logger, brokers []string, group string, topics []string, handle Handler) error {
	cl, err := NewConsumer(brokers, group, topics...)
	if err != nil {
		return err
	}
	defer cl.Close()
	logger.InfoContext(ctx, "consumer started", "group", group, "topics", topics, "brokers", brokers)
	err = Consume(ctx, cl, logger, handle)
	logger.InfoContext(ctx, "consumer stopped", "group", group)
	return err
}
