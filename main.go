package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

var mediaTypes = []string{"IMAGE", "VIDEO", "MEME", "NONE"}

type config struct {
	params       searchParams
	limit        int
	proxy        *url.URL
	output       string
	maxRotations int
	split        bool
}

func main() {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, "adlib:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "adlib:", err)
		os.Exit(1)
	}
}

func parseFlags() (config, error) {
	var cfg config
	var proxy string
	flag.StringVar(&cfg.params.Query, "q", "", "search keywords")
	flag.StringVar(&cfg.params.Country, "c", "US", "2-letter country code")
	flag.StringVar(&cfg.params.Status, "status", "ALL", "ACTIVE, INACTIVE or ALL")
	flag.IntVar(&cfg.params.PageSize, "page", 30, "ads requested per call (Meta returns at most 10)")
	flag.IntVar(&cfg.limit, "n", 100, "maximum ads to collect")
	flag.StringVar(&proxy, "proxy", os.Getenv("ADLIB_PROXY"), "proxy URL http://user:pass@host:port (rotating recommended)")
	flag.StringVar(&cfg.output, "o", "-", "output JSONL file, - for stdout")
	flag.IntVar(&cfg.maxRotations, "rotations", 10, "maximum fresh sessions per slice after rate limits or errors")
	flag.BoolVar(&cfg.split, "split", true, "crawl media type and status slices in parallel")
	flag.Parse()

	if cfg.params.Query == "" {
		return cfg, errors.New("-q is required")
	}
	cfg.params.Country = strings.ToUpper(cfg.params.Country)
	cfg.params.Status = strings.ToUpper(cfg.params.Status)
	cfg.params.MediaType = "ALL"
	cfg.params.PageSize = min(max(cfg.params.PageSize, 1), 30)
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return cfg, fmt.Errorf("proxy: %w", err)
		}
		cfg.proxy = u
	}
	return cfg, nil
}

func slices(cfg config) []searchParams {
	if !cfg.split {
		return []searchParams{cfg.params}
	}
	statuses := []string{cfg.params.Status}
	if cfg.params.Status == "ALL" {
		statuses = []string{"ACTIVE", "INACTIVE"}
	}
	var out []searchParams
	for _, status := range statuses {
		for _, media := range mediaTypes {
			p := cfg.params
			p.Status = status
			p.MediaType = media
			out = append(out, p)
		}
	}
	return out
}

func run(ctx context.Context, cfg config) error {
	out, closeOut, err := openOutput(cfg.output)
	if err != nil {
		return err
	}
	defer closeOut()
	w := bufio.NewWriter(out)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pages := make(chan []json.RawMessage)
	var wg sync.WaitGroup
	for _, p := range slices(cfg) {
		wg.Go(func() { crawl(ctx, cfg, p, pages) })
	}
	go func() {
		wg.Wait()
		close(pages)
	}()

	start := time.Now()
	seen := make(map[string]bool, cfg.limit)
	for ads := range pages {
		added, err := writeAds(w, ads, seen, cfg.limit)
		if err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("flush: %w", err)
		}
		fmt.Fprintf(os.Stderr, "+%d total=%d %.1fs\n", added, len(seen), time.Since(start).Seconds())
		if len(seen) >= cfg.limit {
			cancel()
			break
		}
	}
	fmt.Fprintf(os.Stderr, "done: %d ads in %.1fs\n", len(seen), time.Since(start).Seconds())
	return nil
}

func crawl(ctx context.Context, cfg config, p searchParams, pages chan<- []json.RawMessage) {
	name := p.Status + "/" + p.MediaType
	p.SessionID = newUUID()
	p.Collation = newUUID()

	var c *client
	defer func() {
		if c != nil {
			c.close()
		}
	}()
	cursor := ""
	rotations := 0

	for ctx.Err() == nil {
		var err error
		if c == nil {
			c, err = newClient(ctx, cfg.proxy)
		}
		var pg page
		if err == nil {
			pg, err = c.search(ctx, p, cursor)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if c != nil {
				c.close()
				c = nil
			}
			rotations++
			if rotations > cfg.maxRotations {
				fmt.Fprintf(os.Stderr, "%s: gave up after %d rotations: %v\n", name, cfg.maxRotations, err)
				return
			}
			fmt.Fprintf(os.Stderr, "%s: rotate %d: %v\n", name, rotations, err)
			continue
		}

		select {
		case pages <- pg.Ads:
		case <-ctx.Done():
			return
		}
		if pg.NextCursor == "" {
			fmt.Fprintf(os.Stderr, "%s: exhausted\n", name)
			return
		}
		cursor = pg.NextCursor
	}
}

func writeAds(w io.Writer, ads []json.RawMessage, seen map[string]bool, limit int) (int, error) {
	added := 0
	for _, ad := range ads {
		if len(seen) >= limit {
			break
		}
		var id struct {
			AdArchiveID string `json:"ad_archive_id"`
		}
		if err := json.Unmarshal(ad, &id); err != nil {
			return added, fmt.Errorf("ad id: %w", err)
		}
		if seen[id.AdArchiveID] {
			continue
		}
		seen[id.AdArchiveID] = true
		if _, err := w.Write(append(ad, '\n')); err != nil {
			return added, fmt.Errorf("write: %w", err)
		}
		added++
	}
	return added, nil
}

func openOutput(path string) (io.Writer, func(), error) {
	if path == "-" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("output: %w", err)
	}
	return f, func() { f.Close() }, nil
}
