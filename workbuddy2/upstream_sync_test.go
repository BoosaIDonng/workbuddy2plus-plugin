package main

import (
	"net/http"
	"testing"
	"time"
)

func TestGlobalCatalogUnionKeepsDesktopMetadataAndHostCallback(t *testing.T) {
	sa := syntheticStoredAuth(t, workBuddyRealmGlobal)
	calls := 0
	do := func(req *http.Request, callback string) (*hostHTTPResponse, error) {
		calls++
		if callback != "sync-callback" {
			t.Fatalf("callback=%q", callback)
		}
		if req.URL.Host != "www.workbuddy.ai" || req.Header.Get("Origin") != originRefererGlobal {
			t.Fatalf("routing=%s", req.URL)
		}
		if _, ok := req.Context().Deadline(); !ok {
			t.Fatal("missing deadline")
		}
		body := `{"code":0,"data":{"models":[{"id":"shared","name":"Desktop"},{"id":"desktop-only"}]}}`
		if req.URL.Path != "/v3/config" {
			body = `{"code":0,"data":{"models":[{"id":"enterprise-only"}]}}`
		} else if req.Header.Get("User-Agent") == "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0" {
			body = `{"code":0,"data":{"models":[{"id":"shared","name":"IDE"},{"id":"ide-only"}]}}`
		} else if req.Header.Get("User-Agent") == "CLI/2.63.2 CodeBuddy/2.63.2" {
			body = `{"code":0,"data":{"agents":[{"name":"cli","models":["cli-only"]}]}}`
		}
		return &hostHTTPResponse{StatusCode: 200, Body: []byte(body)}, nil
	}
	got, err := fetchWorkBuddyCatalog(sa, "sync-callback", do)
	if err != nil {
		t.Fatal(err)
	}
	if got.Realm != workBuddyRealmGlobal || got.Endpoint != workBuddyEndpointV3Config || len(got.Models) != 5 {
		t.Fatalf("catalog=%+v", got)
	}
	if got.Models[0].ID != "shared" || got.Models[0].Name != "Desktop" {
		t.Fatalf("desktop precedence=%+v", got.Models[0])
	}
	if calls != 4 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestGlobalCatalogPartialFailureAndNoInventedModels(t *testing.T) {
	for _, success := range []bool{true, false} {
		got, err := fetchWorkBuddyCatalog(syntheticStoredAuth(t, workBuddyRealmGlobal), "", func(req *http.Request, _ string) (*hostHTTPResponse, error) {
			if success && req.Header.Get("User-Agent") == "CLI/2.63.2 CodeBuddy/2.63.2" && req.URL.Path == "/v3/config" {
				return &hostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["only-model"]}]}}`)}, nil
			}
			return &hostHTTPResponse{StatusCode: 503}, nil
		})
		if success {
			if err != nil || len(got.Models) != 1 || got.Models[0].ID != "only-model" {
				t.Fatalf("partial=%+v err=%v", got, err)
			}
		} else if err == nil || len(got.Models) > 0 {
			t.Fatalf("failure invented catalog: %+v err=%v", got, err)
		}
	}
}

func TestDeductionExpiryAndRoutingWindow(t *testing.T) {
	now := time.Now()
	end := now.Add(48 * time.Hour).Truncate(time.Second)
	p := resourcePackage{DeductionEndTime: end.UnixMilli(), ExpiredTime: "later", PackageEndTime: "later", CycleEndTime: "later"}
	if got := resourcePackageExpiry(p); got != end.In(shanghaiZone).Format(time.RFC3339) {
		t.Fatalf("expiry=%q", got)
	}
	p.DeductionEndTime = 0
	if resourcePackageExpiry(p) != "later" {
		t.Fatal("missing fallback")
	}
	cr := &creditsSummary{Packages: []packageSummary{
		{Remain: 20, CycleEnd: end.Format(time.RFC3339)},
		{Remain: 30, CycleEnd: now.Add(time.Hour).In(shanghaiZone).Format("2006-01-02 15:04:05")},
		{Remain: 100, CycleEnd: now.Add(-time.Hour).Format(time.RFC3339)},
		{Remain: 200, CycleEnd: "invalid"},
	}}
	if got := expiringCredits(cr, 7*24*time.Hour); got != 50 {
		t.Fatalf("expiring=%d", got)
	}
}
