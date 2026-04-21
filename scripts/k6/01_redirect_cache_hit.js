// Scenario 1 from docs/benchmarks.md: redirect under cache-hit load.
// Target: sustain 1000 RPS against a small pool of pre-created short codes
// so every request is a cache hit. Measure p50/p99/p999.
//
// Design SLO (README §1 non-functional): p99 < 50ms on cache hit.
// Encoded as a threshold so the run fails loudly on regression.
//
// Run:
//   make load-redirect-hit         # or:
//   k6 run scripts/k6/01_redirect_cache_hit.js

import http from 'k6/http';
import { check, fail } from 'k6';

const BASE_URL  = __ENV.BASE_URL  || 'http://localhost:8080';
const RPS       = parseInt(__ENV.RPS       || '1000', 10);
const DURATION  = __ENV.DURATION  || '30s';
const POOL_SIZE = parseInt(__ENV.POOL_SIZE || '20', 10);

// Pool size affects cache behavior: a larger pool is more realistic for a
// warm-cache benchmark but loses hit rate if the working set exceeds the
// 1h Redis TTL window (won't here — the test runs for seconds).

// setup() runs once, creates POOL_SIZE codes, and returns them to the main VU.
// iter-zero setup body requests go through the URL-validation path, so we
// use example.com which the SSRF guard accepts.
export function setup() {
    const codes = [];
    for (let i = 0; i < POOL_SIZE; i++) {
        const payload = JSON.stringify({
            url: `https://example.com/k6/pool/${Date.now()}-${i}`,
        });
        const res = http.post(`${BASE_URL}/shorten`, payload, {
            headers: { 'content-type': 'application/json' },
        });
        if (res.status !== 201) {
            fail(`setup: POST /shorten -> ${res.status} ${res.body}`);
        }
        codes.push(JSON.parse(res.body).short_code);
    }

    // Warmup: hit each code once so the cache is populated before measurement.
    // These warmup calls are outside the k6 measurement window by virtue of
    // being in setup().
    for (const code of codes) {
        const r = http.get(`${BASE_URL}/${code}`, { redirects: 0 });
        if (r.status !== 302) {
            fail(`warmup: GET /${code} -> ${r.status}`);
        }
    }

    return { codes };
}

export const options = {
    // Do NOT set discardResponseBodies globally — it applies to setup() too,
    // where we need res.body to parse the created short_code out of the
    // 201 response. We throw bodies away in the main VU function via the
    // per-request `responseType: "none"` option below.
    scenarios: {
        redirect_hit: {
            executor: 'constant-arrival-rate',
            rate: RPS,
            timeUnit: '1s',
            duration: DURATION,
            preAllocatedVUs: Math.max(50, Math.ceil(RPS / 20)),
            maxVUs:          Math.max(100, Math.ceil(RPS / 10)),
        },
    },
    thresholds: {
        // Headline SLO from the design doc.
        'http_req_duration{status:302}': ['p(99)<50', 'p(95)<20'],
        // Guardrail: anything other than a 302 on the hot path is a bug.
        'http_req_failed': ['rate<0.001'],
    },
};

export default function (data) {
    // Random choice through the pool so the cache stays hot for every code.
    const code = data.codes[Math.floor(Math.random() * data.codes.length)];
    const res = http.get(`${BASE_URL}/${code}`, {
        redirects: 0,
        responseType: 'none',
        tags: { name: 'redirect' },
    });
    check(res, {
        'status is 302': (r) => r.status === 302,
    });
}
