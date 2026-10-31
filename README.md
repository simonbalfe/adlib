# adlib

A small, fast command-line tool that collects ads from the [Meta Ad Library](https://www.facebook.com/ads/library/) using plain HTTP. It needs no browser, no API key, and no Facebook account.

It writes one ad per line as JSON (JSONL), so you can stop it at any time and keep everything collected so far.

In a test run, collecting 1,000 ads for `nike` in the US took 62 seconds through a rotating residential proxy.

## Install

Requires Go 1.25 or newer.

```bash
go install github.com/simonbalfe/adlib@latest
```

Or build from source:

```bash
git clone https://github.com/simonbalfe/adlib
cd adlib
go build -o adlib .
```

## Usage

```bash
adlib -q "nike" -c US -n 1000 -o nike.jsonl
```

| Flag | Default | Meaning |
|---|---|---|
| `-q` | (required) | Search keywords |
| `-c` | `US` | Two-letter country code |
| `-n` | `100` | Maximum number of ads to collect |
| `-status` | `ALL` | `ACTIVE`, `INACTIVE`, or `ALL` |
| `-o` | `-` | Output file; `-` writes to stdout |
| `-proxy` | `$ADLIB_PROXY` | Proxy URL, e.g. `http://user:pass@host:port` |
| `-split` | `true` | Crawl several slices of the results in parallel |
| `-rotations` | `10` | Fresh sessions allowed per slice after a rate limit or error |
| `-page` | `30` | Ads requested per call (Meta currently returns at most 10) |

Progress goes to stderr; ads go to the output file.

### Use a rotating proxy

Meta rate-limits by IP address. For more than a handful of pages, use a rotating residential proxy:

```bash
export ADLIB_PROXY='http://user:pass@proxy.example.com:1000'
adlib -q "running shoes" -c GB -n 2000 -o shoes.jsonl
```

When a request is rate-limited, the tool opens a new connection. With a rotating proxy, that new connection gets a new IP address, so the crawl carries on without waiting.

### Work with the output

Each line is the full ad object Meta returns, so every field it provides is kept.

```bash
wc -l nike.jsonl
jq -r '[.ad_archive_id, .page_name, .is_active] | @tsv' nike.jsonl
```

## How it works

```mermaid
sequenceDiagram
    participant A as adlib
    participant H as facebook.com
    participant G as facebook.com/api/graphql/
    A->>H: GET / (logged out)
    H-->>A: HTML + cookies
    Note over A: read lsd token, revision and session values from the HTML
    loop until enough ads
        A->>G: POST AdLibrarySearchPaginationQuery (query, country, cursor)
        G-->>A: up to 10 ads + next cursor
    end
```

1. **Session.** The tool loads the logged-out Facebook homepage. The response sets session cookies, and its HTML contains the `lsd` anti-forgery token plus a few build and session values. The Ad Library page itself blocks non-browser clients, but the homepage does not.
2. **Search.** It sends the same GraphQL request the Ad Library website sends when you scroll: `AdLibrarySearchPaginationQuery`, with your query, country and status as variables.
3. **Paging.** Each response returns a batch of ads and a cursor. The next request passes that cursor to get the following batch.
4. **Parallel slices.** One cursor chain can only go as fast as one request at a time. With `-split`, the search is divided into separate slices (active or inactive × image, video, meme or no media), and each slice runs its own chain on its own connection at the same time. The results are combined and duplicates removed by `ad_archive_id`.
5. **Rate limits.** Meta reports a rate limit as GraphQL error `1675004`. The affected slice drops its connection, starts a fresh session, and retries the same cursor.

## Limits

- This uses Meta's internal website API, not a public one. Meta can change it at any time, and the GraphQL query ID in `client.go` may need updating when it does.
- Without a proxy, expect rate limits after a few requests.
- Coverage matches what the Ad Library website shows for the same search.
