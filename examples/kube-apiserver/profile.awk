# Simulated kube-apiserver request latency with a long tail.
# Usage: awk -v url=http://localhost:8081 -v stream=kube-apiserver -v body=<tmpfile> [-v rules=rules.yaml] -f profile.awk
#
# Most requests finish in <100ms, so p50 stays healthy throughout. The tail is
# what moves:
#   6h    normal traffic; WATCH requests are long-running (30-60s) by design
#   3h    etcd compaction: 10 min where 2% of all requests take 1-4s
#   15m   a controller starts unpaginated LIST pods: LIST tail grows to 4% at 2-8s
#   live  the LIST tail continues for ~4 min, then the controller is fixed
BEGIN {
	srand()
	step = 30; steps = 720                                 # 6h of 30s steps
	nb = split("0.05 0.1 0.25 0.5 1 2 4 8 15 30 60", le, " ")
	# Series: verb, resource, requests/s
	ns = split("GET:pods:40 LIST:pods:8 POST:pods:5 PUT:configmaps:10 WATCH:pods:2", spec, " ")
	for (s = 1; s <= ns; s++) {
		split(spec[s], f, ":")
		verb[s] = f[1]; res[s] = f[2]; rps[s] = f[3]
	}

	for (i = 0; i < steps; i++) {
		tick(i, step)
		printf "%s", snapshot(i * step * 1000) > body
	}
	close(body)
	printf "==> backfilling 6h of apiserver traffic\n"
	if (system("curl -sf -X POST '" url "/backfill?align=now' --data-binary @" body " >/dev/null") != 0) {
		print "backfill failed" > "/dev/stderr"; exit 1
	}
	if (rules != "") {
		printf "==> submitting p99 rule and alert, evaluated over the 6h backfill\n"
		if (system("curl -sf -X POST '" url "/rules/kube-apiserver?backfill=6h' -H 'Content-Type: application/yaml' --data-binary @" rules " | sed 's/^/    /'") != 0) {
			print "rule submission failed" > "/dev/stderr"; exit 1
		}
	}

	printf "==> pushing live metrics every 2s (ctrl-c to stop)\n"
	printf "    KubeAPIServerLatencyP99High fires for LIST pods ~2m in, resolves ~4m after the tail ends\n"
	for (t = 0; ; t++) {
		tick(steps + t * 2 / step, 2)
		cmd = "curl -sf -X POST '" url "/push/" stream "' --data-binary @- >/dev/null"
		printf "%s", snapshot(-1) | cmd
		close(cmd)
		printf "\r    t=%ds", t * 2
		system("sleep 2")
	}
}

# tick advances every series by dt seconds at position i (30s steps).
function tick(i, dt,    s, total, tail, lo, hi) {
	for (s = 1; s <= ns; s++) {
		total = int(rps[s] * dt * (0.9 + 0.2 * rand()))
		tail = 0.002; lo = 5; hi = 5                         # 0.2% in the 0.5-1s bucket
		if (i >= steps - 360 && i < steps - 340) { tail = 0.02; lo = 5; hi = 7 }              # etcd compaction
		if (verb[s] == "LIST" && i >= steps - 30 && i < steps + 8) { tail = 0.04; lo = 6; hi = 8 }  # unpaginated LIST
		if (verb[s] == "WATCH") observe(s, total, 10, 11, 1)  # all long-running
		else observe(s, total, lo, hi, tail)
	}
}

# observe spreads total requests: (1-tail) fast, tail spread over buckets lo..hi.
function observe(s, total, lo, hi, tail,    j, fast, c, cum, width) {
	split("0.70 0.20 0.07 0.03", fast, " ")                  # <=50ms, <=100ms, <=250ms, <=500ms
	cum = 0
	width = hi - lo + 1
	for (j = 1; j <= nb; j++) {
		c = 0
		if (j <= 4) c = total * (1 - tail) * fast[j]
		if (j >= lo && j <= hi) c += total * tail / width
		c = int(c + rand())
		cum += c
		bucket[s, j] += cum
		hsum[s] += c * le[j] * 0.7
	}
	count[s] += cum
}

function snapshot(ts,    out, suffix, s, j, lbl) {
	suffix = ts >= 0 ? " " ts : ""
	for (s = 1; s <= ns; s++) {
		lbl = sprintf("verb=\"%s\",resource=\"%s\",scope=\"%s\"", verb[s], res[s], verb[s] == "LIST" ? "cluster" : "namespace")
		for (j = 1; j <= nb; j++)
			out = out sprintf("apiserver_request_duration_seconds_bucket{%s,le=\"%s\"} %d%s\n", lbl, le[j], bucket[s, j], suffix)
		out = out sprintf("apiserver_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d%s\n", lbl, count[s], suffix)
		out = out sprintf("apiserver_request_duration_seconds_sum{%s} %.3f%s\n", lbl, hsum[s], suffix)
		out = out sprintf("apiserver_request_duration_seconds_count{%s} %d%s\n", lbl, count[s], suffix)
		out = out sprintf("apiserver_request_total{%s,code=\"200\"} %d%s\n", lbl, count[s], suffix)
	}
	return out
}
