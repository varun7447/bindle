package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/bindle-ai/bindle/backend/internal/pricesrc"
)

// AWSPriceList serves Amazon Bedrock list prices from AWS's public price
// list (no AWS credentials), cached per region.
type AWSPriceList struct {
	HTTP    *http.Client
	TTL     time.Duration // default 6h
	BaseURL func(offer, region string) string

	mu    sync.Mutex
	cache map[string]awsEntry
}

type awsEntry struct {
	rows []pricesrc.BedrockPrice
	at   time.Time
}

const maxOfferBytes = 64 << 20

// Get returns on-demand token prices for region.
func (a *AWSPriceList) Get(ctx context.Context, region string) ([]pricesrc.BedrockPrice, error) {
	ttl := a.TTL
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	a.mu.Lock()
	if e, ok := a.cache[region]; ok && time.Since(e.at) < ttl {
		a.mu.Unlock()
		return e.rows, nil
	}
	a.mu.Unlock()
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	url := a.BaseURL
	if url == nil {
		url = pricesrc.OfferURL
	}
	var rows []pricesrc.BedrockPrice
	for _, offer := range pricesrc.BedrockOffers {
		u := url(offer, region)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("AWS price list unreachable: %w", err)
		}
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			continue // the offer isn't sold in this region
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("AWS price list returned HTTP %d", resp.StatusCode)
		}
		got, err := pricesrc.ParseBedrockOffer(http.MaxBytesReader(nil, resp.Body, maxOfferBytes), region, u)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}
	if rows == nil {
		rows = []pricesrc.BedrockPrice{}
	}
	a.mu.Lock()
	if a.cache == nil || len(a.cache) > 64 {
		a.cache = map[string]awsEntry{}
	}
	a.cache[region] = awsEntry{rows: rows, at: time.Now()}
	a.mu.Unlock()
	return rows, nil
}

func (s *Server) awsPrices(w http.ResponseWriter, r *http.Request) {
	if s.AWSPrices == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the AWS price list is not configured")
		return
	}
	region := r.URL.Query().Get("region")
	if !pricesrc.RegionRe.MatchString(region) {
		badRequest(w, "region must be an AWS region code such as us-east-1")
		return
	}
	rows, err := s.AWSPrices.Get(r.Context(), region)
	if err != nil {
		s.logger().Warn("aws price list", "region", region, "err", err)
		writeError(w, http.StatusBadGateway, "upstream", "could not read the AWS price list; try again later")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"region": region, "prices": rows})
}
