//go:build integration

package kafka_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
	"github.com/huy205-dev/ticketrush/internal/platform/testenv"
)

var containers testenv.Env

func TestMain(m *testing.M) {
	testenv.Main(m, testenv.Needs{Kafka: true}, &containers)
}

func newTopic(t *testing.T) string {
	t.Helper()
	topic := fmt.Sprintf("test.consume.%d", time.Now().UnixNano())
	if err := kafka.EnsureTopics(context.Background(), containers.Kafka.Client(t), map[string]int32{topic: 1}); err != nil {
		t.Fatal(err)
	}
	return topic
}

func produce(t *testing.T, topic string, values ...string) {
	t.Helper()
	cl := containers.Kafka.Client(t)
	for _, v := range values {
		if err := cl.ProduceSync(context.Background(), &kgo.Record{Topic: topic, Value: []byte(v)}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
}

// seen collects handled values.
type seen struct {
	mu     sync.Mutex
	values []string
}

func (s *seen) add(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = append(s.values, v)
}

func (s *seen) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.values)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// run starts kafka.Run and returns a function that stops it and fails the
// test unless it returns promptly.
func run(t *testing.T, group, topic string, h kafka.Handler) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- kafka.Run(ctx, slog.New(slog.DiscardHandler), containers.Kafka.Brokers, group, []string{topic}, h)
	}()
	return func() {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return after cancel: leaving the group is blocked")
		}
	}
}

// Failing records are retried in place, permanent failures are skipped,
// order is kept, and a restarted member resumes after the committed batch.
func TestConsume(t *testing.T) {
	topic := newTopic(t)
	group := "g-" + topic
	produce(t, topic, "a", "flaky", "bad", "b")

	var got seen
	failures := 0
	stop := run(t, group, topic, func(_ context.Context, r *kgo.Record) error {
		v := string(r.Value)
		switch {
		case v == "flaky" && failures < 2:
			failures++
			return errors.New("temporary")
		case v == "bad":
			return kafka.Permanent(errors.New("cannot process"))
		}
		got.add(v)
		return nil
	})
	waitFor(t, "the first records", func() bool { return len(got.get()) == 3 })
	stop()
	if want := []string{"a", "flaky", "b"}; !slices.Equal(got.get(), want) || failures != 2 {
		t.Fatalf("handled %v after %d failures, want %v after 2", got.get(), failures, want)
	}

	// A new member of the same group starts after the committed offsets.
	produce(t, topic, "c")
	var again seen
	stop = run(t, group, topic, func(_ context.Context, r *kgo.Record) error {
		again.add(string(r.Value))
		return nil
	})
	waitFor(t, "the new record", func() bool { return len(again.get()) >= 1 })
	stop()
	if !slices.Equal(again.get(), []string{"c"}) {
		t.Errorf("restarted member handled %v, want only [c]", again.get())
	}
}

// A handler stuck retrying when the consumer is stopped leaves its batch
// uncommitted: the record comes back to the next member.
func TestConsumeStopMidBatch(t *testing.T) {
	topic := newTopic(t)
	group := "g-" + topic
	produce(t, topic, "x")

	attempts := make(chan struct{}, 100)
	stop := run(t, group, topic, func(context.Context, *kgo.Record) error {
		attempts <- struct{}{}
		return errors.New("always failing")
	})
	<-attempts
	stop()

	var got seen
	stop = run(t, group, topic, func(_ context.Context, r *kgo.Record) error {
		got.add(string(r.Value))
		return nil
	})
	waitFor(t, "the redelivered record", func() bool { return len(got.get()) == 1 })
	stop()
	if !slices.Equal(got.get(), []string{"x"}) {
		t.Errorf("redelivered %v, want [x]", got.get())
	}
}
