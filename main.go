// tairagram: polls public Instagram accounts anonymously and forwards new
// posts to a Discord webhook. State lives in a single state.json committed
// back to the repo by the GitHub Actions workflow.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Loaded from the ACCOUNTS env var (comma-separated) in main. The list is
// deliberately NOT in the source: the repo is public.
var accounts []string

const (
	igOrigin = "https://www.instagram.com"
	ua       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36"

	// The default web app id serves posts, but 400s on accounts carrying a
	// business category. The second one resolves those profiles (id only) so
	// we can fall back to the feed endpoint, which still works for them.
	appIDFeed    = "936619743392459"
	appIDProfile = "238260118697367"

	discordLimit   = 2000
	maxCatchup     = 3 // most posts sent per account per run
	requestTimeout = 20 * time.Second
)

// Overridable so a stubborn account can be pushed past during seeding.
var throttleLimit = envInt("THROTTLE_LIMIT", 2)

// Instagram throttles anonymous requests from datacenter IPs hard: checking
// all 25 accounts in one run gets most of them 401'd. Each run walks a slice
// of the list and leaves a cursor behind, so consecutive runs cover everyone.
// With 6 accounts per run on a 30 minute schedule the whole list is checked
// about every two hours.
// Set in main after loadEnv, so a local .env is honoured.
var (
	batchSize    int
	accountDelay time.Duration
	seedOnly     bool
	webhookURL   string
	stateFile    = filepath.Join(baseDir(), "state.json")
)

// Minimal .env loader for local runs (KEY=VALUE, # comments). Real env vars
// win over the file, which is how the workflow injects secrets in CI. No
// dependency needed for this.
func loadEnv() {
	f, err := os.Open(filepath.Join(baseDir(), ".env"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key != "" && os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

// The repo and its Actions logs are public, so neither may contain account
// names. State keys are short hashes; logs show a masked form.
func acctKey(account string) string {
	sum := sha256.Sum256([]byte(account))
	return hex.EncodeToString(sum[:])[:12]
}

func masked(account string) string {
	n := 3
	if len(account) < n {
		n = len(account)
	}
	return account[:n] + "***"
}

// Go's http.Transport negotiates HTTP/2 automatically over TLS (ALPN), which
// is what the JS version needed a hand-rolled http2 client for: Instagram
// answers HTTP/1.1 requests to this API with 429 regardless of headers.
var client = &http.Client{Timeout: requestTimeout}

func baseDir() string {
	if exe, err := os.Executable(); err == nil && !strings.Contains(exe, "go-build") {
		return filepath.Dir(exe)
	}
	dir, _ := os.Getwd()
	return dir
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// ---------- state ----------

type state struct {
	Cursor int               `json:"cursor"`
	IDs    map[string]string `json:"ids"`  // account -> numeric user id (feed fallback)
	Last   map[string]string `json:"last"` // account -> shortcode of last sent post
}

func loadState() *state {
	s := &state{IDs: map[string]string{}, Last: map[string]string{}}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, s)
	if s.IDs == nil {
		s.IDs = map[string]string{}
	}
	if s.Last == nil {
		s.Last = map[string]string{}
	}
	return s
}

// Saved after every mutation so a crash mid-run loses at most nothing:
// state must be on disk before the run ends, because the workflow commits
// whatever is there even when we exit non-zero.
func (s *state) save() {
	s.Cursor = ((s.Cursor % len(accounts)) + len(accounts)) % len(accounts)
	data, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(stateFile, append(data, '\n'), 0o644)
}

// ---------- instagram ----------

type post struct {
	Shortcode string
	Timestamp int64
	Caption   string
}

type igError struct {
	Status    int
	Throttled bool
	Body      string
}

func (e *igError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, truncate(e.Body, 140))
}

func newIGError(status int, body string) *igError {
	return &igError{
		Status: status,
		Body:   body,
		// 401 means "require_login", 429 means slow down. Both are the same
		// throttle wearing different hats, and both are transient.
		Throttled: status == 401 || status == 429,
	}
}

func igGet(path, appID string, out any) error {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(http.MethodGet, igOrigin+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", ua)
		req.Header.Set("X-IG-App-ID", appID)

		res, err := client.Do(req)
		if err != nil {
			last = err
		} else {
			body, readErr := io.ReadAll(res.Body)
			res.Body.Close()
			if readErr != nil {
				last = readErr
			} else if res.StatusCode == http.StatusOK {
				return json.Unmarshal(body, out)
			} else {
				igErr := newIGError(res.StatusCode, string(body))
				// Retrying a throttle just deepens it, and 400 is Instagram's
				// business-category bug, which no retry clears.
				if igErr.Throttled || igErr.Status == 400 {
					return igErr
				}
				last = igErr
			}
		}
		if attempt == 0 {
			time.Sleep(5 * time.Second)
		}
	}
	return last
}

type profileResponse struct {
	Data struct {
		User *struct {
			ID    string `json:"id"`
			Media struct {
				Edges []struct {
					Node struct {
						Shortcode string `json:"shortcode"`
						TakenAt   int64  `json:"taken_at_timestamp"`
						Captions  struct {
							Edges []struct {
								Node struct {
									Text string `json:"text"`
								} `json:"node"`
							} `json:"edges"`
						} `json:"edge_media_to_caption"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"edge_owner_to_timeline_media"`
		} `json:"user"`
	} `json:"data"`
}

type feedResponse struct {
	Items []struct {
		Code    string `json:"code"`
		TakenAt int64  `json:"taken_at"`
		Caption *struct {
			Text string `json:"text"`
		} `json:"caption"`
	} `json:"items"`
}

// Pinned posts come back first no matter how old they are, so order by date.
// Trusting position 0 means sitting on a pinned post forever.
func normalise(posts []post) []post {
	kept := posts[:0]
	for _, p := range posts {
		if p.Shortcode != "" && p.Timestamp != 0 {
			kept = append(kept, p)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Timestamp > kept[j].Timestamp })
	return kept
}

func fetchViaProfile(st *state, account string) ([]post, error) {
	var resp profileResponse
	err := igGet("/api/v1/users/web_profile_info/?username="+url.QueryEscape(account), appIDFeed, &resp)
	if err != nil {
		return nil, err
	}
	user := resp.Data.User
	if user == nil {
		return nil, fmt.Errorf("no user in profile response")
	}
	if user.ID != "" && st.IDs[acctKey(account)] != user.ID {
		st.IDs[acctKey(account)] = user.ID
		st.save()
	}
	posts := make([]post, 0, len(user.Media.Edges))
	for _, e := range user.Media.Edges {
		caption := ""
		if len(e.Node.Captions.Edges) > 0 {
			caption = e.Node.Captions.Edges[0].Node.Text
		}
		posts = append(posts, post{e.Node.Shortcode, e.Node.TakenAt, caption})
	}
	return normalise(posts), nil
}

// Used only for the accounts whose profile endpoint 400s. Their user id is
// cached so that costs one request per run instead of two.
func fetchViaFeed(st *state, account string) ([]post, error) {
	userID := st.IDs[acctKey(account)]
	if userID == "" {
		var resp profileResponse
		err := igGet("/api/v1/users/web_profile_info/?username="+url.QueryEscape(account), appIDProfile, &resp)
		if err != nil {
			return nil, err
		}
		if resp.Data.User == nil || resp.Data.User.ID == "" {
			return nil, fmt.Errorf("could not resolve user id")
		}
		userID = resp.Data.User.ID
		st.IDs[acctKey(account)] = userID
		st.save()
		time.Sleep(1500 * time.Millisecond)
	}

	var resp feedResponse
	if err := igGet("/api/v1/feed/user/"+userID+"/?count=12", appIDFeed, &resp); err != nil {
		return nil, err
	}
	posts := make([]post, 0, len(resp.Items))
	for _, item := range resp.Items {
		caption := ""
		if item.Caption != nil {
			caption = item.Caption.Text
		}
		posts = append(posts, post{item.Code, item.TakenAt, caption})
	}
	return normalise(posts), nil
}

func fetchPosts(st *state, account string) ([]post, error) {
	posts, err := fetchViaProfile(st, account)
	if err == nil {
		return posts, nil
	}
	var ig *igError
	if !asIGError(err, &ig) || ig.Status != 400 {
		return nil, err
	}
	time.Sleep(1500 * time.Millisecond)
	return fetchViaFeed(st, account)
}

func asIGError(err error, target **igError) bool {
	if e, ok := err.(*igError); ok {
		*target = e
		return true
	}
	return false
}

// ---------- discord ----------

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func buildMessage(account string, p post) string {
	link := "https://www.kkinstagram.com/p/" + p.Shortcode + "/"
	header := fmt.Sprintf("New post from **%s:** %s\n**Date:** <t:%d:F>\n\n", account, link, p.Timestamp)
	quoted := ""
	if p.Caption != "" {
		quoted = "> " + strings.ReplaceAll(p.Caption, "\n", "\n> ")
	}
	if len([]rune(header))+len([]rune(quoted)) <= discordLimit {
		return header + quoted
	}
	return header + truncate(quoted, discordLimit-len([]rune(header))-1) + "…"
}

func sendToDiscord(content string) error {
	payload, _ := json.Marshal(map[string]string{"content": content})

	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "tairagram")

		res, err := client.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if res.StatusCode >= 200 && res.StatusCode < 300 {
			return nil
		}
		if res.StatusCode == http.StatusTooManyRequests {
			wait := 5 * time.Second
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			if json.Unmarshal(body, &rl) == nil && rl.RetryAfter > 0 {
				wait = time.Duration(rl.RetryAfter*1000) * time.Millisecond
			}
			time.Sleep(wait)
			continue
		}
		return fmt.Errorf("Discord HTTP %d: %s", res.StatusCode, truncate(string(body), 140))
	}
	return fmt.Errorf("Discord still rate limiting after 3 attempts")
}

// ---------- main loop ----------

func checkLatestPost(st *state, account string) error {
	posts, err := fetchPosts(st, account)
	if err != nil {
		return err
	}
	if len(posts) == 0 {
		return fmt.Errorf("no posts returned")
	}

	if seedOnly {
		st.Last[acctKey(account)] = posts[0].Shortcode
		st.save()
		fmt.Printf("[%s] seeded at %s\n", masked(account), posts[0].Shortcode)
		return nil
	}

	// No state, or the saved post has scrolled out of the 12 we can see:
	// record where we are rather than dumping an unknown backlog.
	saved := st.Last[acctKey(account)]
	if saved == "" {
		st.Last[acctKey(account)] = posts[0].Shortcode
		st.save()
		fmt.Printf("[%s] first run, seeded at %s\n", masked(account), posts[0].Shortcode)
		return nil
	}
	idx := -1
	for i, p := range posts {
		if p.Shortcode == saved {
			idx = i
			break
		}
	}
	if idx == -1 {
		st.Last[acctKey(account)] = posts[0].Shortcode
		st.save()
		fmt.Printf("[%s] saved post %s no longer in feed, re-seeded at %s\n", masked(account), saved, posts[0].Shortcode)
		return nil
	}

	pending := posts[:idx]
	if len(pending) == 0 {
		fmt.Printf("[%s] up to date\n", masked(account))
		return nil
	}
	if len(pending) > maxCatchup {
		fmt.Printf("[%s] %d new posts, sending newest %d\n", masked(account), len(pending), maxCatchup)
		pending = pending[:maxCatchup]
	}

	// Oldest first, so the channel reads in chronological order.
	for i := len(pending) - 1; i >= 0; i-- {
		if err := sendToDiscord(buildMessage(account, pending[i])); err != nil {
			return err
		}
		// State advances only after Discord accepts it, so a failed send
		// retries next run.
		st.Last[acctKey(account)] = pending[i].Shortcode
		st.save()
		fmt.Printf("[%s] sent %s\n", masked(account), pending[i].Shortcode)
	}
	return nil
}

func main() {
	loadEnv()

	webhookURL = os.Getenv("DISCORD_WEBHOOK_URL")
	if _, err := url.ParseRequestURI(webhookURL); err != nil || !strings.HasPrefix(webhookURL, "https://") {
		fmt.Fprintln(os.Stderr, "fatal: DISCORD_WEBHOOK_URL is not set or not a valid https URL")
		os.Exit(1)
	}

	for _, a := range strings.Split(os.Getenv("ACCOUNTS"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		fmt.Fprintln(os.Stderr, "fatal: ACCOUNTS is not set (comma-separated usernames)")
		os.Exit(1)
	}

	batchSize = envInt("BATCH_SIZE", 6)
	accountDelay = time.Duration(envInt("ACCOUNT_DELAY", 8000)) * time.Millisecond
	seedOnly = os.Getenv("SEED_ONLY") == "1"

	st := loadState()
	start := st.Cursor % len(accounts)
	count := batchSize
	if count > len(accounts) {
		count = len(accounts)
	}
	fmt.Printf("Checking %d of %d accounts, starting at #%d (%s)\n",
		count, len(accounts), start, masked(accounts[start]))

	ok := 0
	consecutiveThrottles := 0
	firstThrottleIndex := -1
	stoppedAt := -1
	var failed []string

	for i := 0; i < count; i++ {
		index := (start + i) % len(accounts)
		account := accounts[index]
		err := checkLatestPost(st, account)
		if err == nil {
			ok++
			consecutiveThrottles = 0
			firstThrottleIndex = -1
		} else {
			fmt.Fprintf(os.Stderr, "[%s] ERROR: %v\n", masked(account), err)
			var ig *igError
			if asIGError(err, &ig) && ig.Throttled {
				consecutiveThrottles++
				if firstThrottleIndex == -1 {
					firstThrottleIndex = index
				}
				if consecutiveThrottles >= throttleLimit {
					// Resume from the FIRST throttled account next run, so
					// the one before the limit tripped isn't starved.
					stoppedAt = firstThrottleIndex
					fmt.Fprintf(os.Stderr,
						"Throttled by Instagram, stopping early. Next run resumes at %s.\n",
						masked(accounts[stoppedAt]))
					break
				}
			} else {
				consecutiveThrottles = 0
				firstThrottleIndex = -1
				failed = append(failed, masked(account))
			}
		}
		if i < count-1 {
			time.Sleep(accountDelay)
		}
	}

	switch {
	case stoppedAt == -1:
		st.Cursor = start + count
	case ok == 0 && stoppedAt == start:
		// Zero accounts got through and we stopped exactly where we started,
		// so resuming here would repeat the identical run forever. Step over
		// the blocker; it comes back around on the next full cycle.
		st.Cursor = stoppedAt + 1
		fmt.Fprintf(os.Stderr, "No progress at %s, skipping it next run.\n", masked(accounts[start]))
	default:
		// Progress was made, so resume at the first throttled account.
		st.Cursor = stoppedAt
	}
	st.save()

	note := ""
	if stoppedAt != -1 {
		note = ", stopped early on throttle"
	}
	fmt.Printf("\n%d checked, %d genuine failure(s)%s\n", ok, len(failed), note)
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "Failed: %s\n", strings.Join(failed, ", "))
	}

	// Throttling is expected background noise, so it must not paint every run
	// red. A non-throttle failure, or a run where nothing got through for
	// reasons OTHER than throttling, is a real problem worth surfacing.
	if len(failed) > 0 || (ok == 0 && stoppedAt == -1) {
		os.Exit(1)
	}
}
