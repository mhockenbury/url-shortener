// Scenario 2 from docs/benchmarks.md: redirect cold start (cache empty).
// Every redirect in the opening seconds is a cache miss, falling through to
// Postgres via singleflight. Goal: measure how long the cold window lasts
// and what p99 looks like during vs after it.
//
// Run:
//   make load-redirect-cold     # or:
//   k6 run scripts/k6/02_redirect_cold_start.js

import http from 'k6/http';
import redis from 'k6/experimental/redis';
import { check, fail } from 'k6';

const BASE_URL  = __ENV.BASE_URL  || 'http://localhost:8080';
const REDIS_URL = __ENV.REDIS_URL || 'redis://localhost:6379';
const RPS       = parseInt(__ENV.RPS       || '1000', 10);
const DURATION  = __ENV.DURATION  || '30s';
const POOL_SIZE = parseInt(__ENV.POOL_SIZE || '20', 10);

// Cold window: how long after start we consider "cold". Anything slower than
// this is a tail latency we want to surface. Tag requests by window so we can
// read per-window thresholds in the summary.
const COLD_WINDOW_MS = parseInt(__ENV.COLD_WINDOW_MS || '2000', 10);

// Shared redis client for setup() cache invalidation. k6's experimental/redis
// is fine here; no long-lived pool needed.
const redisClient = new redis.Client(REDIS_URL);

// setup() creates the pool AND evicts any cached url:<code> entries so the
// first request for each code is guaranteed to miss. We don't FLUSHDB — that
// would nuke the clicks stream and any other keys.
export async function setup() {
    const codes = [];
    for (let i = 0; i < POOL_SIZE; i++) {
        const payload = JSON.stringify({
            url: `https://example.com/k6/cold/${Date.now()}-${i}`,
        });
        const res = http.post(`${BASE_URL}/shorten`, payload, {
            headers: { 'content-type': 'application/json' },
        });
        if (res.status !== 201) {
            fail(`setup: POST /shorten -> ${res.status} ${res.body}`);
        }
        codes.push(JSON.parse(res.body).short_code);
    }

    // Evict this pool's cache entries. Creating via POST /shorten may or may
    // not populate the cache depending on the service's current behavior
    // (it currently does NOT — cache is populated on first redirect). Belt
    // and suspenders: delete anyway so the test is deterministic.
    for (const code of codes) {
        await redisClient.del(`url:${code}`);
    }

    // Record the test start time so per-window thresholds work.
    return { codes, startMillis: Date.now() };
}

export const options = {
    scenarios: {
        redirect_cold: {
            executor: 'constant-arrival-rate',
            rate: RPS,
            timeUnit: '1s',
            duration: DURATION,
            // Slightly fatter pool than scenario 1: cold requests block on DB,
            // so VUs can get tied up while singleflight resolves.
            preAllocatedVUs: Math.max(100, Math.ceil(RPS / 10)),
            maxVUs:          Math.max(200, Math.ceil(RPS /  5)),
        },
    },
    thresholds: {
        // Overall p99 cap: DB-fallback adds work but should still land well
        // under the cache-hit SLO once warm. 50ms matches the design doc.
        'http_req_duration{status:302}': ['p(99)<50'],

        // Steady-state (after the cold window) should look close to scenario 1.
        // p95 < 20ms is the cache-hit target — warm state should match it.
        'http_req_duration{status:302,window:warm}': ['p(95)<20'],

        // No request errors on the hot path.
        'http_req_failed': ['rate<0.001'],
    },
};

export default function (data) {
    const code = data.codes[Math.floor(Math.random() * data.codes.length)];
    const elapsedMs = Date.now() - data.startMillis;
    const window = elapsedMs < COLD_WINDOW_MS ? 'cold' : 'warm';

    const res = http.get(`${BASE_URL}/${code}`, {
        redirects: 0,
        responseType: 'none',
        tags: { name: 'redirect', window },
    });
    check(res, {
        'status is 302': (r) => r.status === 302,
    });
}
