// Package queue is the waiting room (SPEC.md 7.2): buyers join a per-event
// queue in Redis, a worker admits them in join order by issuing admission
// tokens, and booking records which order each token created.
package queue
