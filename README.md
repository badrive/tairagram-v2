# tairagram

Polls public Instagram accounts anonymously and forwards new posts to a
Discord webhook. Runs on GitHub Actions every 30 minutes; state lives in
`state.json`, committed back to the repo after each run.

The repo is public, so nothing identifying is stored in it:
- webhook URL and account list live in **repo secrets** (`.env` only for local runs)
- `state.json` keys are hashes of account names, not the names
- Actions logs (publicly viewable on a public repo) print masked names like `raj***`

## Setup

1. Create the repo (public) and push these files. Never commit `.env` —
   it is gitignored; only `.env.example` belongs in the repo.
2. Repo → Settings → Secrets and variables → Actions → **Secrets** → add:
   - `DISCORD_WEBHOOK_URL` — your webhook
   - `ACCOUNTS` — comma-separated usernames, e.g. `acct_one,acct_two,acct_three`
3. Actions tab → "IG to Discord Scraper" → Run workflow → check
   **seed_only** → run it ceil(N/6) times (N accounts, batches of 6). This
   records every account's current post without spamming the channel.
4. Done — the schedule takes over.

## Local run

    cp .env.example .env   # then fill it in
    go run .

Real environment variables override `.env`. Tuning: `BATCH_SIZE` (default 6),
`ACCOUNT_DELAY` ms between accounts (default 8000), `SEED_ONLY=1` to record
state without posting.

## Notes

- Editing the `ACCOUNTS` secret re-orders the list; the cursor may point at a
  different account for one cycle. Harmless. New accounts seed themselves on
  first contact.
- Runner IPs are Azure datacenter ranges; Instagram throttles them (401/429).
  The batch cursor + early stop in `main.go` exist to survive that.
- GitHub disables the schedule after 60 days without repo activity — if the
  bot goes quiet, check the Actions tab for the banner.
