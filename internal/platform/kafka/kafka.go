// Package kafka creates the franz-go client and the topics the services use.
// Redpanda speaks the Kafka API, so nothing here is Redpanda specific.
package kafka

import (
	"context"
	"errors"
	"fmt"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// NewClient returns a client for brokers. Producing waits for all in-sync
// replicas and is idempotent (the franz-go defaults), so a retried send does
// not duplicate a record within one producer session.
func NewClient(brokers []string, opts ...kgo.Opt) (*kgo.Client, error) {
	cl, err := kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(brokers...)}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return cl, nil
}

// EnsureTopics creates the missing topics with the given partition counts
// and the broker's default replication factor. Existing topics are left as
// they are.
func EnsureTopics(ctx context.Context, cl *kgo.Client, partitions map[string]int32) error {
	adm := kadm.NewClient(cl)
	for topic, n := range partitions {
		resp, err := adm.CreateTopic(ctx, n, -1, nil, topic)
		if err == nil {
			err = resp.Err
		}
		if err != nil && !errors.Is(err, kerr.TopicAlreadyExists) {
			return fmt.Errorf("create topic %s: %w", topic, err)
		}
	}
	return nil
}
