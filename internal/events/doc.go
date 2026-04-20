// Package events wraps the Redis Stream producer/consumer used for click
// analytics. The API uses the producer on the hot redirect path; the worker
// uses the consumer.
package events
