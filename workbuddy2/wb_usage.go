// wb_usage.go implements the panel's "usage" view: per-request credit spend
// from the official billing API (get-user-request-usage), aggregated by model
// / day / client. The upstream client and pagination semantics are ported
// from workbuddy2api (Sep 2025 build); the aggregation endpoint is new.
// Contract: see management.go route /usage — all responses are aggregates,
// never raw credential data.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	wauth "github.com/sliverkiss/workbuddy2plus-plugin/internal/auth"
	wupstream "github.com/sliverkiss/workbuddy2plus-plugin/internal/upstream"
)

const (
	usageDefaultDays = 7
	usageMaxDays     = 31
	// Mirror workbuddy2api's page cap: beyond this the bill is incomplete and
	// we surface an error instead of silently under-reporting credits.
	usageMaxPages = 100
	usagePageSize = 100
)

// usageRecord is one official billing row.
type usageRecord struct {
	RequestID   string  `json:"requestId"`
	RequestTime int64   `json:"requestTimeMs"`
	Credit      float64 `json:"credit"`
	Model       string  `json:"model"`
	Client      string  `json:"client"`
	Agent       string  `json:"agentPurpose"`
}

// parseUsageTime normalizes the upstream requestTime field: numeric seconds,
// numeric milliseconds, or a CST wall-clock string.
func parseUsageTime(raw json.RawMessage) int64 {
	s := strings.Trim(string(raw), `"`)
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		if v < 1e12 {
			return int64(v * 1000)
		}
		return int64(v)
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.FixedZone("CST", 8*3600)); err == nil {
		return t.UnixMilli()
	}
	return 0
}

// fetchUsageRecords pulls [start,end] billing rows for one account. Candidate
// URLs mirror workbuddy2api: billing base with and without /v2, then the
// workbuddy.cn domain; the first candidate returning code==0 wins. Live
// traffic shifts pagination boundaries, so pages are re-read via total and
// rows deduped by requestId (strict total validation always fails here).
func fetchUsageRecords(c *wupstream.Client, a *wauth.Auth, start, end time.Time) ([]usageRecord, error) {
	base := c.BillingBaseFor(a)
	candidates := []string{
		base + "/billing/meter/get-user-request-usage",
		base + "/v2/billing/meter/get-user-request-usage",
		"https://www.workbuddy.cn/billing/meter/get-user-request-usage",
		"https://www.workbuddy.cn/v2/billing/meter/get-user-request-usage",
	}
	const layout = "2006-01-02 15:04:05"
	var records []usageRecord
	var lastErr error
	for _, url := range candidates {
		records = records[:0]
		seen := map[string]bool{}
		ok := true
		fetched := 0
		total := 0
		for pageNum := 1; pageNum <= usageMaxPages; pageNum++ {
			body, _ := json.Marshal(map[string]any{
				"startTime": start.Format(layout),
				"endTime":   end.Format(layout),
				"pageNum":   pageNum,
				"pageSize":  usagePageSize,
			})
			req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
			if err != nil {
				return nil, err
			}
			c.BillingHeaders(req, a)
			origin := "https://" + req.URL.Host
			req.Header.Set("Origin", origin)
			req.Header.Set("Referer", origin+"/profile/plans-usage")
			data, err := c.DoJSON(req)
			if err != nil {
				ok = false
				lastErr = fmt.Errorf("%s: %w", req.URL.Host+req.URL.Path, err)
				break
			}
			var resp struct {
				Total int `json:"total"`
				Rows  []struct {
					RequestID    string          `json:"requestId"`
					RequestTime  json.RawMessage `json:"requestTime"`
					Credit       float64         `json:"credit"`
					Model        string          `json:"model"`
					Client       string          `json:"client"`
					AgentPurpose string          `json:"agentPurpose"`
				} `json:"data"`
			}
			if err := json.Unmarshal(data, &resp); err != nil {
				ok = false
				lastErr = fmt.Errorf("%s parse: %w", req.URL.Host+req.URL.Path, err)
				break
			}
			for _, r := range resp.Rows {
				if r.RequestID != "" && seen[r.RequestID] {
					continue
				}
				if r.RequestID != "" {
					seen[r.RequestID] = true
				}
				records = append(records, usageRecord{
					RequestID:   r.RequestID,
					RequestTime: parseUsageTime(r.RequestTime),
					Credit:      r.Credit,
					Model:       r.Model,
					Client:      r.Client,
					Agent:       r.AgentPurpose,
				})
			}
			fetched += len(resp.Rows)
			total = resp.Total
			if len(resp.Rows) < usagePageSize || fetched >= total {
				break
			}
			if pageNum == usageMaxPages {
				ok = false
				lastErr = fmt.Errorf("usage truncated at %d records (total=%d), narrow the window", len(records), total)
			}
		}
		if ok {
			return records, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("usage fetch failed")
	}
	return nil, lastErr
}

// usageModelStat is the per-model rollup row.
type usageModelStat struct {
	Model    string  `json:"model"`
	Requests int     `json:"requests"`
	Credit   float64 `json:"credit"`
}

// usageDayStat is the per-day credit rollup (Asia/Shanghai calendar days).
type usageDayStat struct {
	Day    string  `json:"day"`
	Count  int     `json:"count"`
	Credit float64 `json:"credit"`
}

// usageClientStat is the per-client rollup row.
type usageClientStat struct {
	Client   string  `json:"client"`
	Requests int     `json:"requests"`
	Credit   float64 `json:"credit"`
}

// usageResponse is the /usage aggregate payload.
type usageResponse struct {
	Days        int               `json:"days"`
	Accounts    int               `json:"accounts"`
	Errors      []string          `json:"errors,omitempty"`
	TotalCredit float64           `json:"total_credit"`
	TotalCount  int               `json:"total_count"`
	ByModel     []usageModelStat  `json:"by_model"`
	ByDay       []usageDayStat    `json:"by_day"`
	ByClient    []usageClientStat `json:"by_client"`
	Recent      []usageRecord     `json:"recent"`
}

var shanghaiZone = time.FixedZone("CST", 8*3600)

// usageDayOf renders a record timestamp as a Shanghai calendar day.
func usageDayOf(ms int64) string {
	if ms <= 0 {
		return "unknown"
	}
	return time.UnixMilli(ms).In(shanghaiZone).Format("2006-01-02")
}

// aggregateUsage rolls raw billing rows into model/day/client views.
func aggregateUsage(records []usageRecord, days int, errs []string) usageResponse {
	resp := usageResponse{Days: days, Errors: errs}
	byModel := map[string]*usageModelStat{}
	byDay := map[string]*usageDayStat{}
	byClient := map[string]*usageClientStat{}
	for _, r := range records {
		resp.TotalCredit += r.Credit
		resp.TotalCount++
		if m, ok := byModel[r.Model]; ok {
			m.Requests++
			m.Credit += r.Credit
		} else {
			byModel[r.Model] = &usageModelStat{Model: r.Model, Requests: 1, Credit: r.Credit}
		}
		day := usageDayOf(r.RequestTime)
		if d, ok := byDay[day]; ok {
			d.Count++
			d.Credit += r.Credit
		} else {
			byDay[day] = &usageDayStat{Day: day, Count: 1, Credit: r.Credit}
		}
		client := strings.TrimSpace(r.Client)
		if client == "" {
			client = "unknown"
		}
		if cl, ok := byClient[client]; ok {
			cl.Requests++
			cl.Credit += r.Credit
		} else {
			byClient[client] = &usageClientStat{Client: client, Requests: 1, Credit: r.Credit}
		}
	}
	for _, m := range byModel {
		resp.ByModel = append(resp.ByModel, *m)
	}
	sort.Slice(resp.ByModel, func(i, j int) bool { return resp.ByModel[i].Credit > resp.ByModel[j].Credit })
	for _, d := range byDay {
		resp.ByDay = append(resp.ByDay, *d)
	}
	sort.Slice(resp.ByDay, func(i, j int) bool { return resp.ByDay[i].Day < resp.ByDay[j].Day })
	for _, cl := range byClient {
		resp.ByClient = append(resp.ByClient, *cl)
	}
	sort.Slice(resp.ByClient, func(i, j int) bool { return resp.ByClient[i].Credit > resp.ByClient[j].Credit })
	// Recent 20 rows, newest first.
	recent := append([]usageRecord(nil), records...)
	sort.Slice(recent, func(i, j int) bool { return recent[i].RequestTime > recent[j].RequestTime })
	if len(recent) > 20 {
		recent = recent[:20]
	}
	resp.Recent = recent
	return resp
}

// usageDaysParam validates the days query parameter (default 7, cap 31).
func usageDaysParam(query urlValues) int {
	days := usageDefaultDays
	if v := strings.TrimSpace(query.Get("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			days = n
		}
	}
	if days < 1 {
		days = 1
	}
	if days > usageMaxDays {
		days = usageMaxDays
	}
	return days
}

// urlValues is the minimal query accessor used by handleUsageQuery so tests
// can drive it without net/url plumbing.
type urlValues interface {
	Get(key string) string
}

// handleUsageQuery aggregates billing rows for every non-disabled account.
// Per-account fetches run concurrently (sem=3) to bound upstream load; a
// failed account is skipped and reported in errors[] (never blocks others).
func handleUsageQuery(req pluginapi.ManagementRequest) (int, any) {
	days := usageDaysParam(req.Query)
	if days < 1 || days > usageMaxDays {
		return http.StatusBadRequest, map[string]any{"error": "invalid days"}
	}
	files, err := panelHostAuthList()
	if err != nil {
		return http.StatusBadGateway, map[string]any{"error": "auth list unavailable"}
	}
	end := time.Now()
	start := end.AddDate(0, 0, -days+1)
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, shanghaiZone)

	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		all   []usageRecord
		errs  []string
		acctN int
	)
	sem := make(chan struct{}, 3)
	for _, f := range files {
		if f.Disabled {
			continue
		}
		wg.Add(1)
		go func(f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sa, _, err := hostAuthGetBundle(f.AuthIndex)
			if err != nil {
				mu.Lock()
				errs = append(errs, shortName(f)+": load auth failed")
				mu.Unlock()
				return
			}
			a := wbAuth(nil, sa)
			if a.UID == "" {
				mu.Lock()
				errs = append(errs, shortName(f)+": no uid")
				mu.Unlock()
				return
			}
			sem <- struct{}{}
			records, err := fetchUsageRecords(wbClient, a, start, end)
			<-sem
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, shortName(f)+": "+sanitizeUpstreamError(err))
				return
			}
			all = append(all, records...)
			acctN++
		}(f)
	}
	wg.Wait()
	resp := aggregateUsage(all, days, errs)
	resp.Accounts = acctN
	return http.StatusOK, resp
}

// shortName renders an account label for error rows (uid8 or file name).
func shortName(f pluginapi.HostAuthFileEntry) string {
	if f.AuthIndex != "" {
		return f.AuthIndex[:minLen(len(f.AuthIndex), 8)]
	}
	return f.Name
}

// sanitizeUpstreamError reduces an internal error to a bounded message for
// management responses (defense in depth on top of redaction upstream).
func sanitizeUpstreamError(err error) string {
	return truncateRedacted(strings.ReplaceAll(err.Error(), "\n", " "), 200)
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}
