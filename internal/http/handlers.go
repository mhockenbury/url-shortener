package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mhockenbury/url-shortener/internal/shortener"
)

// Service is the narrow surface handlers need from the link service.
// Defined here so handler tests can substitute a fake without pulling in
// pgx / redis / clickhouse. The real implementation is *shortener.LinkService.
type Service interface {
	Create(ctx context.Context, in shortener.CreateInput) (shortener.CreateResult, error)
	Lookup(ctx context.Context, shortCode string) (shortener.LookupResult, error)
	Stats(ctx context.Context, shortCode string, since, until time.Time) (shortener.StatsResult, error)
	PublishClick(ctx context.Context, shortCode, referrer, userAgent string) error
}

// Handlers wires the Service + supporting config into HTTP handlers. The
// Resolver is used by URL validation; tests can inject a fake DNS answer.
type Handlers struct {
	svc       Service
	resolver  Resolver
	baseURL   string // used to build the returned short_url; e.g. "http://localhost:8080"
	pingPG    func(context.Context) error
	pingRedis func(context.Context) error
}

// NewHandlers constructs the handler bundle. baseURL is prepended to the
// short_code in responses; pass "http://localhost:8080" or whatever the
// service is reachable on. The two ping fns are used by /healthz; pass
// nil for either to skip that dependency in the health check.
func NewHandlers(
	svc Service,
	resolver Resolver,
	baseURL string,
	pingPG func(context.Context) error,
	pingRedis func(context.Context) error,
) *Handlers {
	if resolver == nil {
		resolver = DefaultResolver
	}
	return &Handlers{
		svc:       svc,
		resolver:  resolver,
		baseURL:   baseURL,
		pingPG:    pingPG,
		pingRedis: pingRedis,
	}
}

// ---- POST /shorten ----

type shortenRequest struct {
	URL       string     `json:"url"`
	Alias     string     `json:"alias,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type shortenResponse struct {
	ShortCode string     `json:"short_code"`
	ShortURL  string     `json:"short_url"`
	LongURL   string     `json:"long_url"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Shorten handles POST /shorten.
//
//	@Summary		Create a short code for a long URL
//	@Description	Validates the URL (scheme, length, SSRF guard) and returns a short code. Supply `alias` to request a specific code; `expires_at` for a TTL.
//	@Tags			links
//	@Accept			json
//	@Produce		json
//	@Param			request	body		shortenRequest	true	"Shorten request"
//	@Success		201		{object}	shortenResponse
//	@Failure		400		{object}	errorResponse	"invalid URL, scheme, alias, or expiry"
//	@Failure		409		{object}	errorResponse	"alias already taken"
//	@Failure		500		{object}	errorResponse	"internal error"
//	@Router			/shorten [post]
func (h *Handlers) Shorten(w http.ResponseWriter, r *http.Request) {
	var req shortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if err := ValidateTarget(req.URL, h.resolver); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Alias != "" && !shortener.IsValidCode(req.Alias) {
		writeError(w, http.StatusBadRequest, "alias must be base62 characters only")
		return
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		writeError(w, http.StatusBadRequest, "expires_at must be in the future")
		return
	}

	res, err := h.svc.Create(r.Context(), shortener.CreateInput{
		LongURL:   req.URL,
		Alias:     req.Alias,
		ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		if errors.Is(err, shortener.ErrAliasTaken) {
			writeError(w, http.StatusConflict, "alias already taken")
			return
		}
		slog.ErrorContext(r.Context(), "create link", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusCreated, shortenResponse{
		ShortCode: res.ShortCode,
		ShortURL:  h.baseURL + "/" + res.ShortCode,
		LongURL:   res.LongURL,
		ExpiresAt: res.ExpiresAt,
	})
}

// ---- GET /{code} ----

// Redirect handles GET /{code}.
// Uses 302 (Found) rather than 301 so browsers re-hit the service on each
// click and analytics stays accurate. See docs/tradeoffs.md.
//
//	@Summary		Redirect a short code to its long URL
//	@Description	Looks up the code (cache then DB), emits a click event on the side, and returns a 302. Expired links return 410.
//	@Tags			links
//	@Param			code	path		string	true	"base62 short code"
//	@Success		302		{string}	string	"Location header set to long URL"
//	@Failure		400		{object}	errorResponse	"invalid code"
//	@Failure		404		{object}	errorResponse	"not found"
//	@Failure		410		{object}	errorResponse	"expired"
//	@Failure		500		{object}	errorResponse	"internal error"
//	@Router			/{code} [get]
func (h *Handlers) Redirect(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if !shortener.IsValidCode(code) {
		writeError(w, http.StatusBadRequest, "invalid code")
		return
	}

	res, err := h.svc.Lookup(r.Context(), code)
	if err != nil {
		switch {
		case errors.Is(err, shortener.ErrNotFound):
			writeError(w, http.StatusNotFound, "not found")
		case errors.Is(err, shortener.ErrExpired):
			writeError(w, http.StatusGone, "link expired")
		default:
			slog.ErrorContext(r.Context(), "redirect lookup", "code", code, "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// Emit click event best-effort in the background so the redirect returns
	// immediately. Detach from the request context so a cancelled request
	// doesn't drop the event mid-flight.
	go func(code, ref, ua string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := h.svc.PublishClick(ctx, code, ref, ua); err != nil {
			slog.Warn("publish click", "code", code, "err", err)
		}
	}(code, r.Referer(), r.UserAgent())

	http.Redirect(w, r, res.LongURL, http.StatusFound)
}

// ---- GET /stats/{code} ----

// StatsWindow is how far back Stats queries. Matches the plan in
// docs/architecture.md — last 7 days of hourly buckets.
const StatsWindow = 7 * 24 * time.Hour

type statsResponse struct {
	ShortCode string                    `json:"short_code"`
	Total     uint64                    `json:"total"`
	Since     time.Time                 `json:"since"`
	Until     time.Time                 `json:"until"`
	Hourly    []shortener.HourlyBucket  `json:"hourly"`
}

// Stats handles GET /stats/{code}. Returns total clicks and per-hour
// buckets over the last StatsWindow. Assumes the code exists without
// verifying against Postgres — an unknown code simply returns zeros,
// which is cheap and avoids a second DB round-trip on every stats call.
//
//	@Summary		Click analytics for a short code
//	@Description	Returns total clicks and per-hour buckets over the last 7 days. Unknown codes return zeros (no Postgres lookup).
//	@Tags			links
//	@Produce		json
//	@Param			code	path		string	true	"base62 short code"
//	@Success		200		{object}	statsResponse
//	@Failure		400		{object}	errorResponse	"invalid code"
//	@Failure		503		{object}	errorResponse	"stats backend unavailable"
//	@Failure		500		{object}	errorResponse	"internal error"
//	@Router			/stats/{code} [get]
func (h *Handlers) Stats(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	if !shortener.IsValidCode(code) {
		writeError(w, http.StatusBadRequest, "invalid code")
		return
	}

	until := time.Now().UTC()
	since := until.Add(-StatsWindow)

	res, err := h.svc.Stats(r.Context(), code, since, until)
	if err != nil {
		if errors.Is(err, shortener.ErrStatsUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "stats backend unavailable")
			return
		}
		slog.ErrorContext(r.Context(), "stats query", "code", code, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, statsResponse{
		ShortCode: code,
		Total:     res.Total,
		Since:     since,
		Until:     until,
		Hourly:    res.Hourly,
	})
}

// ---- GET /healthz ----

type healthResponse struct {
	Status   string `json:"status"`
	Postgres string `json:"postgres,omitempty"`
	Redis    string `json:"redis,omitempty"`
}

// Health pings each configured dependency and returns 200 if all are up,
// 503 if any are down. Handy for readiness probes.
//
//	@Summary		Health check
//	@Description	Pings Postgres and Redis (ClickHouse not yet included). Returns 200 when both respond, 503 otherwise.
//	@Tags			ops
//	@Produce		json
//	@Success		200	{object}	healthResponse
//	@Failure		503	{object}	healthResponse
//	@Router			/healthz [get]
func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{Status: "ok"}
	code := http.StatusOK

	if h.pingPG != nil {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := h.pingPG(ctx); err != nil {
			resp.Status = "degraded"
			resp.Postgres = "down: " + err.Error()
			code = http.StatusServiceUnavailable
		} else {
			resp.Postgres = "ok"
		}
	}
	if h.pingRedis != nil {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := h.pingRedis(ctx); err != nil {
			resp.Status = "degraded"
			resp.Redis = "down: " + err.Error()
			code = http.StatusServiceUnavailable
		} else {
			resp.Redis = "ok"
		}
	}
	writeJSON(w, code, resp)
}

// ---- helpers ----

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorResponse{Error: msg})
}
