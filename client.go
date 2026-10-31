package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const (
	homeURL       = "https://www.facebook.com/"
	graphqlURL    = "https://www.facebook.com/api/graphql/"
	searchDocID   = "25464068859919530"
	searchName    = "AdLibrarySearchPaginationQuery"
	rateLimitCode = 1675004
)

type browser struct {
	name      string
	profile   profiles.ClientProfile
	userAgent string
	secCHUA   string
	platform  string
}

var browsers = []browser{
	{
		name:      "chrome146-mac",
		profile:   profiles.Chrome_146,
		userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
		secCHUA:   `"Chromium";v="146", "Not-A.Brand";v="24", "Google Chrome";v="146"`,
		platform:  `"macOS"`,
	},
	{
		name:      "chrome144-win",
		profile:   profiles.Chrome_144,
		userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36",
		secCHUA:   `"Not(A:Brand";v="8", "Chromium";v="144", "Google Chrome";v="144"`,
		platform:  `"Windows"`,
	},
	{
		name:      "firefox148-mac",
		profile:   profiles.Firefox_148,
		userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:148.0) Gecko/20100101 Firefox/148.0",
	},
	{
		name:      "safari16-mac",
		profile:   profiles.Safari_16_0,
		userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Safari/605.1.15",
	},
}

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
	http    tlsclient.HttpClient
	browser browser
	tokens  map[string]string
	req     int
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
	b := browsers[mathrand.IntN(len(browsers))]
	options := []tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(b.profile),
		tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
		tlsclient.WithTimeoutSeconds(30),
	}
	if proxy != nil {
		options = append(options, tlsclient.WithProxyUrl(proxy.String()))
	}
	hc, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("tls client: %w", err)
	}
	c := &client{http: hc, browser: b}
	if err := c.bootstrap(ctx); err != nil {
		hc.CloseIdleConnections()
		return nil, fmt.Errorf("%s: %w", b.name, err)
	}
	return c, nil
}

func (c *client) close() {
	c.http.CloseIdleConnections()
}

func (c *client) bootstrap(ctx context.Context) error {
	home, _ := url.Parse(homeURL)
	c.http.SetCookies(home, []*http.Cookie{{Name: "wd", Value: "1920x1080"}, {Name: "dpr", Value: "1"}})

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, homeURL, nil)
	if err != nil {
		return err
	}
	c.setNavigationHeaders(req.Header)
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

func (c *client) setNavigationHeaders(h http.Header) {
	setOrdered(h, c.browser, [][2]string{
		{"accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
		{"accept-language", "en-US,en;q=0.9"},
		{"cache-control", "max-age=0"},
		{"sec-fetch-dest", "document"},
		{"sec-fetch-mode", "navigate"},
		{"sec-fetch-site", "none"},
		{"sec-fetch-user", "?1"},
		{"upgrade-insecure-requests", "1"},
	})
}

func (c *client) setGraphQLHeaders(h http.Header, p searchParams) {
	setOrdered(h, c.browser, [][2]string{
		{"accept", "*/*"},
		{"accept-language", "en-US,en;q=0.9"},
		{"content-type", "application/x-www-form-urlencoded"},
		{"origin", "https://www.facebook.com"},
		{"referer", "https://www.facebook.com/ads/library/?active_status=" + strings.ToLower(p.Status) + "&ad_type=all&country=" + p.Country + "&q=" + url.QueryEscape(p.Query)},
		{"sec-fetch-dest", "empty"},
		{"sec-fetch-mode", "cors"},
		{"sec-fetch-site", "same-origin"},
		{"x-asbd-id", c.token("asbd", "359341")},
		{"x-fb-friendly-name", searchName},
		{"x-fb-lsd", c.tokens["lsd"]},
	})
}

func setOrdered(h http.Header, b browser, headers [][2]string) {
	if b.secCHUA != "" {
		headers = append(headers,
			[2]string{"sec-ch-ua", b.secCHUA},
			[2]string{"sec-ch-ua-mobile", "?0"},
			[2]string{"sec-ch-ua-platform", b.platform},
		)
	}
	headers = append(headers, [2]string{"user-agent", b.userAgent})
	order := make([]string, 0, len(headers))
	for _, kv := range headers {
		h.Set(kv[0], kv[1])
		order = append(order, kv[0])
	}
	h[http.HeaderOrderKey] = order
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
