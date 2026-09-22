// overview.go builds the panel overview aggregate for GET /overview.
// Data sources are the existing dashboard build (accounts, credits summary)
// and host auth state — no additional upstream calls. Mirrors the GUI
// Dashboard's stat tiles within the plugin's data surface (see
// docs/panel-migration-api-contract.md §2.1).
package main

import (
	"strings"
	"time"
)

type overviewAttention struct {
	AuthIndex string   `json:"auth_index"`
	Nickname  string   `json:"nickname,omitempty"`
	UID8      string   `json:"uid8,omitempty"`
	Region    string   `json:"region,omitempty"`
	Reasons   []string `json:"reasons"`
	Error     string   `json:"error,omitempty"`
}

type overviewSummary struct {
	Total          int                 `json:"total"`
	Healthy        int                 `json:"healthy"`
	Cooling        int                 `json:"cooling"`
	Disabled       int                 `json:"disabled"`
	Exhausted      int                 `json:"exhausted"`
	InFlightFull   int                 `json:"in_flight_full"`
	Credits        map[string]any      `json:"credits"`
	NeedsAttention []overviewAttention `json:"needs_attention"`
	ServerTime     string              `json:"server_time"`
}

// uid8Of returns the first 8 runes of uid ("" when uid is shorter than 8).
func uid8Of(uid string) string {
	if len(uid) < 8 {
		return ""
	}
	return uid[:8]
}

// isCoolingStatus reports whether the host status string means the account is
// temporarily out of rotation (the host marks keepalive/checkin flows this way).
func isCoolingStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "cooling", "rate_limited":
		return true
	}
	return false
}

// accountReasons derives attention reasons for one dashboard account. The
// host does not expose per-account cooling windows to the plugin, so "cooling"
// is inferred from the host status string (keepalive/checkin flows mark it).
func accountReasons(a wbAccount) []string {
	var reasons []string
	if a.Disabled {
		reasons = append(reasons, "disabled")
	}
	if a.Exhausted {
		reasons = append(reasons, "exhausted")
	}
	if strings.TrimSpace(a.Error) != "" {
		reasons = append(reasons, "error")
	}
	if isCoolingStatus(a.Status) {
		reasons = append(reasons, "cooling")
	}
	return reasons
}

// buildOverviewFromAccounts aggregates one dashboard snapshot into overview
// tiles. s is the already-computed credits summary from summarizeCredits.
func buildOverviewFromAccounts(accounts []wbAccount, s map[string]any) overviewSummary {
	ov := overviewSummary{
		Total:      len(accounts),
		ServerTime: time.Now().Format("2006-01-02 15:04:05"),
	}
	remain, used, size, known, packs := 0, 0, 0, 0, 0
	if s != nil {
		remain = intOf(s["total_remain"])
		used = intOf(s["total_used"])
		size = intOf(s["total_size"])
		known = intOf(s["known_count"])
		packs = intOf(s["pack_count"])
	}
	ov.Credits = map[string]any{
		"remain": remain,
		"used":   used,
		"size":   size,
		"known":  known,
		"packs":  packs,
	}
	for _, a := range accounts {
		switch {
		case a.Disabled:
			ov.Disabled++
		case a.Exhausted:
			ov.Exhausted++
		case isCoolingStatus(a.Status):
			// Checked before Healthy: a cooling account carries no Error, so the
			// old ordering counted it as healthy and left Cooling at 0 forever.
			ov.Cooling++
		case strings.TrimSpace(a.Error) == "":
			ov.Healthy++
		}
		reasons := accountReasons(a)
		if len(reasons) > 0 {
			ov.NeedsAttention = append(ov.NeedsAttention, overviewAttention{
				AuthIndex: a.AuthIndex,
				Nickname:  a.Nickname,
				UID8:      uid8Of(a.UID),
				Region:    a.Region,
				Reasons:   reasons,
				Error:     a.Error,
			})
		}
	}
	return ov
}

// intOf extracts a non-negative int from the loosely-typed summary map.
func intOf(v any) int {
	switch x := v.(type) {
	case int:
		if x < 0 {
			return 0
		}
		return x
	case int64:
		if x < 0 {
			return 0
		}
		return int(x)
	}
	return 0
}
