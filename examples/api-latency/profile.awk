# Simulated API traffic: backfills 24h of history, then pushes live every 2s.
# Usage: awk -v url=http://localhost:8081 -v stream=api -v body=<tmpfile> -f profile.awk
#
# Timeline (relative to now):
#   24h   diurnal traffic: ~20 GET/s and ~5 POST/s, peaking mid-day
#   18h   bad deploy: 35% GET 500s and slow responses for 20 min, then rollback
#    6h   dependency outage: 60% POST 503s for 8 min
#   30m   slow burn: GET 500s ramp from 0.5% to 20%, latency degrades
#   live  the slow burn continues for ~3 min, then recovers
BEGIN {
	srand()
	step = 30; steps = 2880                 # 24h of 30s steps
	split("0.05 0.1 0.25 0.5 1", le, " ")   # histogram bucket bounds (+Inf implied)
	nb = 5

	# body: temp file path passed in by run.sh
	for (i = 0; i < steps; i++) {
		tick(i, step)
		printf "%s", snapshot(i * step * 1000) > body
	}
	close(body)
	printf "==> backfilling 24h of API traffic\n"
	if (system("curl -sf -X POST '" url "/backfill?align=now' --data-binary @" body " >/dev/null") != 0) {
		print "backfill failed" > "/dev/stderr"; exit 1
	}

	printf "==> pushing live metrics every 2s (ctrl-c to stop)\n"
	printf "    HighErrorRate and HighLatencyP99 fire ~2m in, then resolve after the slow burn ends (~3m)\n"
	for (t = 0; ; t++) {
		tick(steps + t * 2 / step, 2)
		cmd = "curl -sf -X POST '" url "/push/" stream "' --data-binary @- >/dev/null"
		printf "%s", snapshot(-1) | cmd
		close(cmd)
		printf "\r    GET 200=%d 500=%d | POST 200=%d 503=%d", n["GET", 200], n["GET", 500], n["POST", 200], n["POST", 503]
		system("sleep 2")
	}
}

# tick advances all counters by one interval of dt seconds at position i,
# where i counts 30s steps (possibly fractional) from the start of the backfill.
function tick(i, dt,    d, get, post, e500, e503, frac) {
	d = 1 + 0.5 * sin(2 * 3.14159 * i / steps)      # diurnal curve
	get = int(20 * dt * d * (0.9 + 0.2 * rand()))
	post = int(5 * dt * d * (0.9 + 0.2 * rand()))
	e500 = 0.005; e503 = 0.002; slow = 0    # slow is global, read by observe

	if (i >= steps - 2160 && i < steps - 2120) { e500 = 0.35; slow = 0.8 }      # bad deploy
	if (i >= steps - 720 && i < steps - 704) { e503 = 0.6 }                      # dependency outage
	if (i >= steps - 60 && i < steps) {                                          # slow burn ramp
		frac = (i - (steps - 60)) / 60
		e500 = 0.005 + 0.195 * frac; slow = 0.7 * frac
	}
	if (i >= steps && i < steps + 6) { e500 = 0.2; slow = 0.7 }                  # live: ~3m (6 steps)

	add("GET", get, e500, 0.002)
	add("POST", post, e500 / 4, e503)
	observe(get + post)
}

function add(method, total, e500, e503,    c500, c503) {
	c500 = int(total * e500 + rand())
	c503 = int(total * e503 + rand())
	n[method, 500] += c500
	n[method, 503] += c503
	n[method, 200] += total - c500 - c503
}

# observe records total requests in the latency histogram. slow (0..1) shifts
# the distribution from mostly <100ms towards 0.5s-1s+.
function observe(total,    j, fast, slowd, mids, c, cum) {
	split("0.55 0.30 0.10 0.04 0.01 0.00", fast, " ")
	split("0.05 0.10 0.25 0.30 0.20 0.10", slowd, " ")
	split("0.03 0.07 0.17 0.37 0.75 1.5", mids, " ")    # typical latency per bucket
	cum = 0
	for (j = 1; j <= nb + 1; j++) {
		c = int(total * ((1 - slow) * fast[j] + slow * slowd[j]) + rand())
		cum += c
		if (j <= nb) buckets[j] += cum
		hsum += c * mids[j]
	}
	hcount += cum
}

# snapshot returns every series in text format. ts < 0 omits timestamps.
function snapshot(ts,    out, suffix, m, s, a, b, j) {
	suffix = ts >= 0 ? " " ts : ""
	split("GET POST", m, " ")
	split("200 500 503", s, " ")
	for (a = 1; a <= 2; a++)
		for (b = 1; b <= 3; b++)
			out = out sprintf("http_requests_total{method=\"%s\",status=\"%s\"} %d%s\n", m[a], s[b], n[m[a], s[b]], suffix)
	for (j = 1; j <= nb; j++)
		out = out sprintf("http_request_duration_seconds_bucket{le=\"%s\"} %d%s\n", le[j], buckets[j], suffix)
	out = out sprintf("http_request_duration_seconds_bucket{le=\"+Inf\"} %d%s\n", hcount, suffix)
	out = out sprintf("http_request_duration_seconds_sum %.3f%s\n", hsum, suffix)
	out = out sprintf("http_request_duration_seconds_count %d%s\n", hcount, suffix)
	return out
}
