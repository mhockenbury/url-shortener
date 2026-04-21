package http

import "time"

// requestTimeout bounds per-request processing across all routes.
const requestTimeout = 5 * time.Second
