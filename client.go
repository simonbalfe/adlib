package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	homeURL       = "https://www.facebook.com/"
	graphqlURL    = "https://www.facebook.com/api/graphql/"
	searchDocID   = "25464068859919530"
	searchName    = "AdLibrarySearchPaginationQuery"
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
	secCHUA       = `"Not:A-Brand";v="99", "Google Chrome";v="145", "Chromium";v="145"`
	rateLimitCode = 1675004
)

var errRateLimited = errors.New("rate limited")

var tokenPatterns = map[string]*regexp.Regexp{
	"lsd":         regexp.MustCompile(`"LSD",\[\],\{"token":"([^"]+)"`),
	"__spin_r":    regexp.MustCompile(`"__spin_r":(\d+)`),
	"__spin_t":    regexp.MustCompile(`"__spin_t":(\d+)`),
	"__spin_b":    regexp.MustCompile(`"__spin_b":"([^"]+)"`),
	"__hsi":       regexp.MustCompile(`"hsi":"(\d+)"`),
	"__hs":        regexp.MustCompile(`"__hs":"([^"]+)"`),
	"__dyn":       regexp.MustCompile(`"__dyn":"([^"]+)"`),
	"__csr":       regexp.MustCompile(`"__csr":"([^"]+)"`),
	"__hsdp":      regexp.MustCompile(`"__hsdp":"([^"]+)"`),
	"__hblp":      regexp.MustCompile(`"__hblp":"([^"]+)"`),
	"__comet_req": regexp.MustCompile(`"__comet_req":(\d+)`),
	"fb_dtsg":     regexp.MustCompile(`"DTSGInitialData",\[\],\{"token":"([^"]+)"`),
	"v":           regexp.MustCompile(`"v"\s*:\s*"([a-f0-9]{4,10})"`),
	"asbd":        regexp.MustCompile(`"asbd_id"\s*:\s*"?(\d+)"?`),
	"jazoest":     regexp.MustCompile(`"jazoest["\s:]+(\d+)`),
}

type client struct {
	http   *http.Client
	tokens map[string]string
	req    int
}

type searchParams struct {
	Query     string
	Country   string
	Status    string
	MediaType string
	Sort      string
	PageSize  int
	SessionID string
	Collation string
}

type page struct {
	Ads        []json.RawMessage
	NextCursor string
}

func newClient(ctx context.Context, proxy *url.URL) (*client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:               http.ProxyURL(proxy),
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	c := &client{http: &http.Client{Jar: jar, Transport: transport, Timeout: 30 * time.Second}}
	if err := c.bootstrap(ctx); err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	return c, nil
}

func (c *client) close() {
	c.http.CloseIdleConnections()
}

func (c *client) bootstrap(ctx context.Context) error {
	home, _ := url.Parse(homeURL)
	c.http.Jar.SetCookies(home, []*http.Cookie{{Name: "wd", Value: "1920x1080"}, {Name: "dpr", Value: "1"}})

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, homeURL, nil)
	if err != nil {
		return err
	}
	setNavigationHeaders(req.Header)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("bootstrap read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bootstrap: http %d", resp.StatusCode)
	}

	c.tokens = extractTokens(string(body))
	if c.tokens["lsd"] == "" {
		return errors.New("bootstrap: no lsd token in homepage")
	}
	return nil
}

func extractTokens(html string) map[string]string {
	tokens := make(map[string]string, len(tokenPatterns))
	for name, re := range tokenPatterns {
		if m := re.FindStringSubmatch(html); m != nil {
			tokens[name] = m[1]
		}
	}
	return tokens
}

func (c *client) search(ctx context.Context, p searchParams, cursor string) (page, error) {
	form := c.payload(p, cursor)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlURL, strings.NewReader(form.Encode()))
	if err != nil {
		return page{}, err
	}
	c.setGraphQLHeaders(req.Header, p)

	resp, err := c.http.Do(req)
	if err != nil {
		return page{}, fmt.Errorf("graphql: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return page{}, fmt.Errorf("graphql read: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return page{}, errRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		return page{}, fmt.Errorf("graphql: http %d", resp.StatusCode)
	}
	return parsePage(body)
}

func (c *client) payload(p searchParams, cursor string) url.Values {
	c.req++
	variables := map[string]any{
		"activeStatus":           p.Status,
		"adType":                 "ALL",
		"bylines":                []string{},
		"collationToken":         p.Collation,
		"contentLanguages":       []string{},
		"countries":              []string{p.Country},
		"excludedIDs":            []string{},
		"first":                  p.PageSize,
		"isTargetedCountry":      false,
		"location":               nil,
		"mediaType":              p.MediaType,
		"multiCountryFilterMode": nil,
		"pageIDs":                []string{},
		"potentialReachInput":    []string{},
		"publisherPlatforms":     []string{},
		"queryString":            p.Query,
		"regions":                []string{},
		"searchType":             "KEYWORD_UNORDERED",
		"sessionID":              p.SessionID,
		"source":                 nil,
		"startDate":              nil,
		"v":                      c.token("v", "fbece7"),
		"viewAllPageID":          "0",
		"sortData":               map[string]string{"direction": p.Sort, "mode": "SORT_BY_TOTAL_IMPRESSIONS"},
	}
	if cursor != "" {
		variables["cursor"] = cursor
	}
	vars, _ := json.Marshal(variables)

	lsd := c.tokens["lsd"]
	form := url.Values{
		"av":                       {"0"},
		"__aaid":                   {"0"},
		"__user":                   {"0"},
		"__a":                      {"1"},
		"__req":                    {strconv.FormatInt(int64(c.req), 36)},
		"__hs":                     {c.token("__hs", "")},
		"dpr":                      {"1"},
		"__ccg":                    {"GOOD"},
		"__rev":                    {c.token("__spin_r", "")},
		"__s":                      {randomHex(3) + ":" + randomHex(3) + ":" + randomHex(3)},
		"__hsi":                    {c.token("__hsi", strconv.FormatInt(time.Now().UnixMilli(), 10))},
		"__comet_req":              {c.token("__comet_req", "94")},
		"lsd":                      {lsd},
		"jazoest":                  {c.token("jazoest", jazoest(lsd))},
		"__spin_r":                 {c.token("__spin_r", "")},
		"__spin_b":                 {c.token("__spin_b", "trunk")},
		"__spin_t":                 {c.token("__spin_t", strconv.FormatInt(time.Now().Unix(), 10))},
		"__jssesw":                 {"1"},
		"fb_api_caller_class":      {"RelayModern"},
		"fb_api_req_friendly_name": {searchName},
		"server_timestamps":        {"true"},
		"variables":                {string(vars)},
		"doc_id":                   {searchDocID},
	}
	for _, name := range []string{"__dyn", "__csr", "fb_dtsg", "__hsdp", "__hblp"} {
		if v := c.tokens[name]; v != "" {
			form.Set(name, v)
		}
	}
	return form
}

func (c *client) token(name, fallback string) string {
	if v := c.tokens[name]; v != "" {
		return v
	}
	return fallback
}

func parsePage(body []byte) (page, error) {
	body = []byte(strings.TrimPrefix(string(body), "for (;;);"))
	var resp struct {
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Data *struct {
			Main struct {
				Results struct {
					Edges []struct {
						Node struct {
							Collated []json.RawMessage `json:"collated_results"`
						} `json:"node"`
					} `json:"edges"`
					PageInfo struct {
						HasNext   bool   `json:"has_next_page"`
						EndCursor string `json:"end_cursor"`
					} `json:"page_info"`
				} `json:"search_results_connection"`
			} `json:"ad_library_main"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return page{}, fmt.Errorf("decode: %w", err)
	}
	for _, e := range resp.Errors {
		if e.Code == rateLimitCode || strings.Contains(strings.ToLower(e.Message), "rate limit") {
			return page{}, errRateLimited
		}
	}
	if resp.Data == nil {
		return page{}, fmt.Errorf("graphql errors: %+v", resp.Errors)
	}

	results := resp.Data.Main.Results
	var p page
	for _, edge := range results.Edges {
		p.Ads = append(p.Ads, edge.Node.Collated...)
	}
	if results.PageInfo.HasNext {
		p.NextCursor = results.PageInfo.EndCursor
	}
	return p, nil
}

func setNavigationHeaders(h http.Header) {
	h.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	h.Set("accept-language", "en-US,en;q=0.9")
	h.Set("cache-control", "max-age=0")
	h.Set("dpr", "1")
	h.Set("sec-ch-ua", secCHUA)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"macOS"`)
	h.Set("sec-fetch-dest", "document")
	h.Set("sec-fetch-mode", "navigate")
	h.Set("sec-fetch-site", "none")
	h.Set("sec-fetch-user", "?1")
	h.Set("upgrade-insecure-requests", "1")
	h.Set("user-agent", userAgent)
	h.Set("viewport-width", "1920")
}

func (c *client) setGraphQLHeaders(h http.Header, p searchParams) {
	h.Set("accept", "*/*")
	h.Set("accept-language", "en-US,en;q=0.9")
	h.Set("content-type", "application/x-www-form-urlencoded")
	h.Set("origin", "https://www.facebook.com")
	h.Set("referer", "https://www.facebook.com/ads/library/?active_status="+strings.ToLower(p.Status)+"&ad_type=all&country="+p.Country+"&q="+url.QueryEscape(p.Query))
	h.Set("sec-ch-ua", secCHUA)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"macOS"`)
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "same-origin")
	h.Set("user-agent", userAgent)
	h.Set("x-asbd-id", c.token("asbd", "359341"))
	h.Set("x-fb-friendly-name", searchName)
	h.Set("x-fb-lsd", c.tokens["lsd"])
}

func jazoest(lsd string) string {
	sum := 0
	for _, r := range lsd {
		sum += int(r)
	}
	return "2" + strconv.Itoa(sum)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
