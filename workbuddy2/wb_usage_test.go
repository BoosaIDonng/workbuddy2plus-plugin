package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	wauth "github.com/sliverkiss/workbuddy2plus-plugin/internal/auth"
	wupstream "github.com/sliverkiss/workbuddy2plus-plugin/internal/upstream"
)

func TestFetchUsageRecordsFetchesPagesConcurrentlyWithoutDroppingRows(t *testing.T) {
	var requests atomic.Int32
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			previous := maxInFlight.Load()
			if current <= previous || maxInFlight.CompareAndSwap(previous, current) {
				break
			}
		}

		var body struct {
			Page int `json:"pageNum"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.Page > 1 {
			time.Sleep(25 * time.Millisecond)
		}
		rows := make([]map[string]any, 0, usagePageSize)
		count := usagePageSize
		if body.Page == 3 {
			count = 50
		}
		for i := 0; i < count; i++ {
			id := (body.Page-1)*usagePageSize + i
			rows = append(rows, map[string]any{
				"requestId":   fmt.Sprintf("request-%d", id),
				"requestTime": 1750000000 + id,
				"credit":      1,
				"model":       "model-a",
				"client":      "client-a",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"total": 250, "data": rows},
		})
	}))
	defer server.Close()

	client := wupstream.New()
	client.BillingBaseCN = server.URL
	auth := &wauth.Auth{UID: "uid-1", AccessToken: "token-1"}
	records, err := fetchUsageRecords(client, auth, time.Unix(1750000000, 0), time.Unix(1750100000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 250 {
		t.Fatalf("got %d records, want 250", len(records))
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("made %d page requests, want 3", got)
	}
	if got := maxInFlight.Load(); got < 2 {
		t.Fatalf("page requests were not concurrent, max in flight=%d", got)
	}
}

func TestUsageCacheHonorsTTLAndForceRefresh(t *testing.T) {
	usageCache.Lock()
	old := usageCache.byDays
	usageCache.byDays = nil
	usageCache.Unlock()
	t.Cleanup(func() {
		usageCache.Lock()
		usageCache.byDays = old
		usageCache.Unlock()
	})

	want := usageResponse{Days: 7, TotalCount: 3}
	storeUsageCache(7, want)
	got, ok := cachedUsage(7, false)
	if !ok || got.TotalCount != want.TotalCount {
		t.Fatalf("cache miss or wrong response: ok=%v got=%+v", ok, got)
	}
	if _, ok := cachedUsage(7, true); ok {
		t.Fatal("force refresh unexpectedly used cached usage")
	}
}
