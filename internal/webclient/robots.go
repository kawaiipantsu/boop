package webclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// robots.txt cache bounds.
const (
	// defaultRobotsTTL is how long a successfully fetched robots.txt is
	// reused. Long enough to keep a multi-page crawl to one extra request,
	// short enough that an operator's change takes effect the same session.
	defaultRobotsTTL = 30 * time.Minute
	// negativeRobotsTTL applies to a conservative deny caused by a 5xx, so a
	// transient server error does not lock a host out for half an hour.
	negativeRobotsTTL = 5 * time.Minute
	// defaultRobotsMaxEntries bounds the cache.
	defaultRobotsMaxEntries = 256
	// maxRobotsBytes caps a robots.txt. Google's own limit is 500 KiB.
	maxRobotsBytes int64 = 512 << 10
)

// Robots is a parsed robots.txt file.
type Robots struct {
	groups []robotsGroup
	// malformed records that parsing found nothing usable. A robots.txt
	// nobody can parse is treated as absent, which means "allowed".
	malformed bool
}

type robotsGroup struct {
	agents     []string
	rules      []robotsRule
	crawlDelay time.Duration
	hasDelay   bool
}

type robotsRule struct {
	allow   bool
	pattern string
}

// ParseRobots parses robots.txt content. It never fails: anything it cannot
// understand is skipped, because a syntax error in a remote file must not stop
// Boop from working.
func ParseRobots(data []byte) *Robots {
	r := &Robots{}
	var cur *robotsGroup
	// startingGroup tracks whether the last significant line was a
	// User-agent, so consecutive agent lines join one group.
	startingGroup := false
	sawDirective := false

	for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "user-agent", "useragent":
			if value == "" {
				continue
			}
			sawDirective = true
			if !startingGroup || cur == nil {
				r.groups = append(r.groups, robotsGroup{})
				cur = &r.groups[len(r.groups)-1]
				startingGroup = true
			}
			cur.agents = append(cur.agents, strings.ToLower(value))
		case "disallow", "allow":
			sawDirective = true
			startingGroup = false
			if cur == nil {
				// Rules before any User-agent line: treat them as a
				// wildcard group rather than discard them.
				r.groups = append(r.groups, robotsGroup{agents: []string{"*"}})
				cur = &r.groups[len(r.groups)-1]
			}
			if key == "disallow" && value == "" {
				// "Disallow:" with no path means nothing is disallowed.
				continue
			}
			if value == "" {
				continue
			}
			cur.rules = append(cur.rules, robotsRule{allow: key == "allow", pattern: value})
		case "crawl-delay":
			sawDirective = true
			startingGroup = false
			if cur == nil {
				continue
			}
			if secs, err := strconv.ParseFloat(value, 64); err == nil && secs >= 0 {
				cur.crawlDelay = time.Duration(secs * float64(time.Second))
				cur.hasDelay = true
			}
		default:
			// sitemap, host, clean-param and unknown keys are ignored, and
			// do not break the current group.
		}
	}
	r.malformed = !sawDirective && len(data) > 0
	return r
}

// group selects the record that applies to agent: the most specific matching
// User-agent, falling back to "*".
func (r *Robots) group(agent string) *robotsGroup {
	a := strings.ToLower(agent)
	var best *robotsGroup
	bestLen := -1
	var wildcard *robotsGroup
	for i := range r.groups {
		g := &r.groups[i]
		for _, name := range g.agents {
			if name == "*" {
				if wildcard == nil {
					wildcard = g
				}
				continue
			}
			// RFC 9309: the product token matches case-insensitively; the
			// longest matching token wins.
			if strings.HasPrefix(a, name) && len(name) > bestLen {
				best, bestLen = g, len(name)
			}
		}
	}
	if best != nil {
		return best
	}
	return wildcard
}

// Allowed reports whether agent may fetch path. The longest matching rule wins
// and Allow beats Disallow on a tie, which is the de-facto standard behaviour
// and RFC 9309 §2.2.2.
func (r *Robots) Allowed(agent, path string) bool {
	if r == nil || r.malformed {
		return true
	}
	g := r.group(agent)
	if g == nil {
		return true
	}
	if path == "" {
		path = "/"
	}
	allowLen, denyLen := -1, -1
	for _, rule := range g.rules {
		n, ok := robotsMatch(rule.pattern, path)
		if !ok {
			continue
		}
		if rule.allow {
			if n > allowLen {
				allowLen = n
			}
		} else if n > denyLen {
			denyLen = n
		}
	}
	// A disallow only wins when it is strictly more specific than the best
	// matching allow; equal specificity resolves in favour of fetching.
	return denyLen <= allowLen || denyLen < 0
}

// CrawlDelay returns the Crawl-delay for agent, or zero when unset.
func (r *Robots) CrawlDelay(agent string) time.Duration {
	if r == nil {
		return 0
	}
	if g := r.group(agent); g != nil && g.hasDelay {
		return g.crawlDelay
	}
	return 0
}

// robotsMatch reports whether pattern matches path, and the pattern's
// specificity (its length ignoring wildcards). "*" matches any run of
// characters and a trailing "$" anchors the end of the path.
func robotsMatch(pattern, path string) (int, bool) {
	p := pattern
	anchored := strings.HasSuffix(p, "$")
	if anchored {
		p = p[:len(p)-1]
	}
	score := len(strings.ReplaceAll(p, "*", ""))
	if !strings.Contains(p, "*") {
		if anchored {
			return score, path == p
		}
		return score, strings.HasPrefix(path, p)
	}
	parts := strings.Split(p, "*")
	pos := 0
	if !strings.HasPrefix(path, parts[0]) {
		return 0, false
	}
	pos = len(parts[0])
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			if i == len(parts)-1 && anchored {
				return score, true // trailing "*$" matches anything
			}
			continue
		}
		if i == len(parts)-1 && anchored {
			if strings.HasSuffix(path[pos:], part) {
				return score, true
			}
			return 0, false
		}
		idx := strings.Index(path[pos:], part)
		if idx < 0 {
			return 0, false
		}
		pos += idx + len(part)
	}
	if anchored && pos != len(path) {
		return 0, false
	}
	return score, true
}

// robotsEntry is one cached host policy.
type robotsEntry struct {
	robots  *Robots
	denyAll bool
	reason  string
	expires time.Time
}

// robotsCache caches parsed robots.txt per host with a TTL and a size bound.
type robotsCache struct {
	mu      sync.Mutex
	entries map[string]robotsEntry
	ttl     time.Duration
	max     int
	now     func() time.Time
}

func newRobotsCache(ttl time.Duration, max int, now func() time.Time) *robotsCache {
	if ttl <= 0 {
		ttl = defaultRobotsTTL
	}
	if max <= 0 {
		max = defaultRobotsMaxEntries
	}
	if now == nil {
		now = time.Now
	}
	return &robotsCache{entries: make(map[string]robotsEntry), ttl: ttl, max: max, now: now}
}

func (c *robotsCache) get(key string) (robotsEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return robotsEntry{}, false
	}
	if c.now().After(e.expires) {
		delete(c.entries, key)
		return robotsEntry{}, false
	}
	return e, true
}

func (c *robotsCache) put(key string, e robotsEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		c.evictLocked()
	}
	c.entries[key] = e
}

// evictLocked drops expired entries, then the entry closest to expiry if the
// cache is still full.
func (c *robotsCache) evictLocked() {
	now := c.now()
	for k, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) < c.max {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, e := range c.entries {
		if oldestKey == "" || e.expires.Before(oldest) {
			oldestKey, oldest = k, e.expires
		}
	}
	delete(c.entries, oldestKey)
}

// size reports the number of live entries. Used by tests.
func (c *robotsCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// checkRobots enforces robots.txt for u.
//
// Policy for the awkward cases, stated explicitly because reasonable people
// choose differently:
//
//   - 2xx: parse and honour it.
//   - 404 or any other 4xx: no policy published, everything is allowed.
//   - Unparseable content: treated as absent, so allowed.
//   - 5xx or 429: the host is saying "not now". RFC 9309 §2.3.1.4 calls for
//     treating an unreachable robots.txt as a full disallow, so Boop refuses
//     the fetch and caches that refusal briefly rather than pushing through.
//   - Transport failure (DNS, connection reset, timeout): allowed. This is a
//     deliberate deviation from the RFC. A host that cannot serve robots.txt
//     at all usually cannot serve the page either, so the fetch will fail on
//     its own merits, and refusing every host whose robots.txt request was
//     reset by a middlebox would break far more than it protects.
//
// Crawl-delay is applied to the per-host rate limiter. A delay longer than
// DefaultMaxCrawlDelay is refused rather than clamped: silently waiting less
// than asked would be ignoring the very file we are honouring.
func (c *Client) checkRobots(ctx context.Context, u *url.URL) error {
	key := hostKey(u)
	entry, ok := c.robots.get(key)
	if !ok {
		entry = c.fetchRobots(ctx, u, key)
		c.robots.put(key, entry)
	}
	target := u.Redacted()
	if entry.denyAll {
		return newError(KindRobotsDenied, "robots", target,
			"robots.txt is unavailable (%s); treating the host as disallowed", entry.reason)
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	if !entry.robots.Allowed(c.agentToken, path) {
		return newError(KindRobotsDenied, "robots", target,
			"robots.txt on %s disallows %s for %s", u.Host, path, c.agentToken)
	}
	if d := entry.robots.CrawlDelay(c.agentToken); d > 0 {
		if d > DefaultMaxCrawlDelay {
			return newError(KindRobotsDenied, "robots", target,
				"robots.txt asks for a %s crawl delay, which exceeds the %s limit", d, DefaultMaxCrawlDelay)
		}
		c.limiter.setInterval(u.Host, d)
	}
	return nil
}

// fetchRobots retrieves and parses /robots.txt for a host.
//
// Two concurrent first requests to the same host may both fetch robots.txt.
// That is a wasted request, not a correctness problem, and it avoids holding
// the cache lock across a network call.
func (c *Client) fetchRobots(ctx context.Context, u *url.URL, key string) robotsEntry {
	robotsURL := key + "/robots.txt"
	resp, err := c.do(ctx, request{
		op:         "robots",
		method:     http.MethodGet,
		rawURL:     robotsURL,
		accept:     "text/plain,*/*;q=0.5",
		skipRobots: true,
		maxBytes:   maxRobotsBytes,
	})
	now := c.now()
	switch {
	case err == nil:
		return robotsEntry{robots: ParseRobots(resp.Body), expires: now.Add(c.robots.ttl)}
	case errors.Is(err, ErrBadStatus):
		var werr *Error
		errors.As(err, &werr)
		if werr.Status >= 500 || werr.Status == http.StatusTooManyRequests {
			return robotsEntry{
				denyAll: true,
				reason:  "HTTP " + strconv.Itoa(werr.Status),
				expires: now.Add(negativeRobotsTTL),
			}
		}
		// 404 and friends: no policy published.
		return robotsEntry{robots: &Robots{}, expires: now.Add(c.robots.ttl)}
	case errors.Is(err, ErrTooLarge):
		// Oversized robots.txt: no usable policy, do not block the host.
		return robotsEntry{robots: &Robots{}, expires: now.Add(c.robots.ttl)}
	case errors.Is(err, ErrBlocked), errors.Is(err, ErrDisabled), errors.Is(err, ErrCancelled):
		// The page fetch is about to fail for the same reason; cache nothing.
		return robotsEntry{robots: &Robots{}, expires: now}
	default:
		return robotsEntry{robots: &Robots{}, expires: now.Add(negativeRobotsTTL)}
	}
}
