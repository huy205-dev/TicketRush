//go:build integration

// Package testkafka gives integration tests a real Kafka API (Redpanda, as
// in deploy/compose.yaml). One container is started per test binary.
package testkafka

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/redpanda"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
)

// Image matches deploy/compose.yaml.
const Image = "redpandadata/redpanda:v26.2.3"

// Kafka is a running Redpanda container.
type Kafka struct {
	container *redpanda.Container
	Brokers   []string
}

// Start launches the container (single core, 1 GB, dev-container mode).
func Start(ctx context.Context) (*Kafka, error) {
	c, err := redpanda.Run(ctx, Image)
	if err != nil {
		return nil, fmt.Errorf("start redpanda: %w", err)
	}
	broker, err := c.KafkaSeedBroker(ctx)
	if err != nil {
		return nil, fmt.Errorf("redpanda seed broker: %w", err)
	}
	return &Kafka{container: c, Brokers: []string{broker}}, nil
}

// Terminate stops the container.
func (k *Kafka) Terminate() {
	if err := testcontainers.TerminateContainer(k.container); err != nil {
		fmt.Fprintf(os.Stderr, "testkafka: terminate: %v\n", err)
	}
}

// Client returns a client closed when the test ends.
func (k *Kafka) Client(t testing.TB, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	cl, err := kafka.NewClient(k.Brokers, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	return cl
}
