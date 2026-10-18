package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestParseEnterpriseCreditsStrict(t *testing.T) {
	got, err := parseEnterpriseCredits([]byte(`{"code":0,"data":{"credit":12.6,"limitNum":100,"cycleStartTime":"a","cycleEndTime":"b"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalUsed != 13 || got.TotalSize != 100 || got.TotalRemain != 87 || got.PackCount != 1 {
		t.Fatalf("credits = %#v", got)
	}
	if len(got.Packages) != 1 || got.Packages[0].Name != "Enterprise" || got.Packages[0].CycleStart != "a" || got.Packages[0].CycleEnd != "b" {
		t.Fatalf("package = %#v", got.Packages)
	}

	for _, raw := range []string{
		`{"data":{"credit":1,"limitNum":100}}`,
		`{"code":null,"data":{"credit":1,"limitNum":100}}`,
		`{"code":0,"data":{"credit":1}}`,
		`{"code":0,"data":{"credit":-1,"limitNum":100}}`,
		`{"code":0,"data":{"credit":"NaN","limitNum":100}}`,
		`{"code":0,"data":{"credit":1,"limitNum":0}}`,
		`{"code":0,"data":{"credit":1,"limitNum":9223372036854775807}}`,
		`{"code":7,"msg":"denied","data":{"credit":1,"limitNum":100}}`,
	} {
		if _, err := parseEnterpriseCredits([]byte(raw)); err == nil {
			t.Errorf("accepted invalid enterprise response %s", raw)
		}
	}
}

func TestParseEnterpriseCreditsRejectsInt64Overflow(t *testing.T) {
	got, err := parseEnterpriseCredits([]byte(`{"code":0,"data":{"credit":1,"limitNum":9223372036854775808}}`))
	if err == nil {
		t.Fatalf("accepted unrepresentable enterprise credits: %#v", got)
	}
}

func TestEnterpriseCreditsFallbackOnlyOn404(t *testing.T) {
	if !errors.Is(classifyEnterpriseHTTPStatus(http.StatusNotFound), errEnterpriseCreditsUnsupported) {
		t.Fatal("404 must allow personal fallback")
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		if errors.Is(classifyEnterpriseHTTPStatus(status), errEnterpriseCreditsUnsupported) {
			t.Fatalf("status %d incorrectly allows fallback", status)
		}
	}
}

func TestFetchUserResourceEnterpriseSelection(t *testing.T) {
	cases := []struct {
		name              string
		config            string
		domain            string
		enterpriseStatus  int
		wantPaths         []string
		wantError         bool
		wantRemain        int64
		wantUsed          int64
		wantSize          int64
		enterpriseHeaders bool
	}{
		{
			name:              "enabled CN uses enterprise snapshot without personal addition",
			config:            "enterprise_credits: true\n",
			domain:            "www.codebuddy.cn",
			wantPaths:         []string{"/billing/meter/get-enterprise-user-usage"},
			wantRemain:        87,
			wantUsed:          13,
			wantSize:          100,
			enterpriseHeaders: true,
		},
		{
			name:       "disabled CN uses personal packages",
			config:     "enterprise_credits: false\n",
			domain:     "www.codebuddy.cn",
			wantPaths:  []string{"/v2/billing/meter/get-user-resource"},
			wantRemain: 40,
			wantUsed:   60,
			wantSize:   100,
		},
		{
			name:       "Global always uses personal packages",
			config:     "enterprise_credits: true\n",
			domain:     "www.workbuddy.ai",
			wantPaths:  []string{"/v2/billing/meter/get-user-resource"},
			wantRemain: 40,
			wantUsed:   60,
			wantSize:   100,
		},
		{
			name:             "CN falls back to personal only on enterprise 404",
			config:           "enterprise_credits: true\n",
			domain:           "www.codebuddy.cn",
			enterpriseStatus: http.StatusNotFound,
			wantPaths:        []string{"/billing/meter/get-enterprise-user-usage", "/v2/billing/meter/get-user-resource"},
			wantRemain:       40,
			wantUsed:         60,
			wantSize:         100,
		},
		{
			name:             "CN does not fall back on enterprise 500",
			config:           "enterprise_credits: true\n",
			domain:           "www.codebuddy.cn",
			enterpriseStatus: http.StatusInternalServerError,
			wantPaths:        []string{"/billing/meter/get-enterprise-user-usage"},
			wantError:        true,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			var enterpriseHeader http.Header
			var enterpriseBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case "/billing/meter/get-enterprise-user-usage":
					enterpriseHeader = r.Header.Clone()
					enterpriseBody, _ = io.ReadAll(r.Body)
					if tt.enterpriseStatus != 0 {
						w.WriteHeader(tt.enterpriseStatus)
						return
					}
					_, _ = w.Write([]byte(`{"code":0,"data":{"credit":12.6,"limitNum":100}}`))
				case "/v2/billing/meter/get-user-resource":
					_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"TotalCount":1,"Accounts":[{"PackageName":"personal","CycleCapacityRemain":40,"CycleCapacitySize":100}]}}}}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			restoreCN := setBillingBase(srv.URL)
			defer restoreCN()
			restoreGlobal := setBillingBaseGlobal(srv.URL)
			defer restoreGlobal()

			cfg, err := parseFeatureRuntime([]byte(tt.config))
			if err != nil {
				t.Fatal(err)
			}
			oldCfg := featureRuntime.Load()
			featureRuntime.Store(cfg)
			t.Cleanup(func() { featureRuntime.Store(oldCfg) })

			cr, err := fetchUserResource(&storedAuth{
				Auth:    storedTokens{AccessToken: "token", Domain: tt.domain},
				Account: storedAccount{UID: "user-1", EnterpriseID: "enterprise-1"},
			})
			if tt.wantError {
				if err == nil {
					t.Fatal("expected enterprise fetch error")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if cr.TotalRemain != tt.wantRemain || cr.TotalUsed != tt.wantUsed || cr.TotalSize != tt.wantSize {
					t.Fatalf("credits = %#v", cr)
				}
			}
			if !reflect.DeepEqual(paths, tt.wantPaths) {
				t.Fatalf("paths = %v, want %v", paths, tt.wantPaths)
			}
			if tt.enterpriseHeaders {
				if enterpriseHeader.Get("Authorization") != "Bearer token" || enterpriseHeader.Get("Accept") != "application/json, text/plain, */*" || enterpriseHeader.Get("Content-Type") != "application/json" || enterpriseHeader.Get("X-Client-Platform") != "web" || enterpriseHeader.Get("X-User-Id") != "user-1" || enterpriseHeader.Get("X-Enterprise-Id") != "enterprise-1" || enterpriseHeader.Get("X-Tenant-Id") != "enterprise-1" || enterpriseHeader.Get("X-Domain") != "www.codebuddy.cn" || enterpriseHeader.Get("Origin") != "https://www.codebuddy.cn" || enterpriseHeader.Get("Referer") != "https://www.codebuddy.cn/" {
					t.Fatalf("enterprise headers = %#v", enterpriseHeader)
				}
				var body map[string]any
				if err := json.Unmarshal(enterpriseBody, &body); err != nil || len(body) != 0 {
					t.Fatalf("enterprise body = %q, err = %v", enterpriseBody, err)
				}
			}
		})
	}
}

func TestReconcileSkipsLifecycleWhenCreditsRefreshFailed(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "token", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "user-stale"},
	}
	oldGetBundle := reconcileHostAuthGetBundle
	reconcileHostAuthGetBundle = func(string) (*storedAuth, *hostAuthPhysical, error) {
		return sa, &hostAuthPhysical{}, nil
	}
	t.Cleanup(func() { reconcileHostAuthGetBundle = oldGetBundle })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/billing/meter/get-enterprise-user-usage" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	restoreBillingBase := setBillingBase(srv.URL)
	defer restoreBillingBase()

	cfg, err := parseFeatureRuntime([]byte("enterprise_credits: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	oldCfg := featureRuntime.Load()
	featureRuntime.Store(cfg)
	t.Cleanup(func() { featureRuntime.Store(oldCfg) })

	const authID = "enterprise-stale-lifecycle"
	accountCache.Store(authID, &accountCacheEntry{
		credits: &creditsSummary{TotalUsed: 100, TotalSize: 100},
		fetched: time.Now().Add(-time.Hour),
	})
	t.Cleanup(func() { accountCache.Delete(authID) })

	action, err := reconcileOneAccount("auth-index", authID, true)
	if err != nil {
		t.Fatal(err)
	}
	if action != lifecycleNone {
		t.Fatalf("action = %s, want none after credits refresh failure", action.String())
	}
}
