// Package storage contains adapters for Postgres (source of truth) and
// Redis (cache + event stream). Domain code depends on interfaces defined
// here, not on the adapters directly.
package storage
