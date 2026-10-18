package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func markupStreamFixture(t *testing.T) (string, []byte) {
	t.Helper()
	const markup = `<｜｜DSML｜｜ calls><｜｜DSML｜｜ invoke name="run"></｜｜DSML｜｜ invoke></｜｜DSML｜｜ calls>`
	var stream strings.Builder
	for _, part := range []string{"answer " + markup[:18], markup[18:]} {
		raw, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": part}}}})
		if err != nil {
			t.Fatal(err)
		}
		stream.WriteString("data: " + string(raw) + "\n\n")
	}
	stream.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	return stream.String(), []byte(`{"tools":[{"type":"function","function":{"name":"run"}}]}`)
}

func TestCPASynchronousMarkupBridges(t *testing.T) {
	stream, body := markupStreamFixture(t)
	for _, framed := range []bool{false, true} {
		chunks, err := aggregateSSEWithCollector(strings.NewReader(stream), framed, nil, body)
		if err != nil {
			t.Fatal(err)
		}
		var wire strings.Builder
		for _, chunk := range chunks {
			payload := string(chunk.Payload)
			if framed && !strings.HasPrefix(payload, "data: ") {
				t.Fatalf("missing SSE framing: %q", payload)
			}
			wire.WriteString(payload)
		}
		if got := wire.String(); strings.Contains(got, "DSML") || !strings.Contains(got, `"name":"run"`) || !strings.Contains(got, `"finish_reason":"tool_calls"`) || !strings.Contains(got, `"content":"answer "`) {
			t.Fatalf("framed=%v, unexpected stream: %s", framed, got)
		}
	}
	result, err := wbAggregate(strings.NewReader(stream), "test-model", body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(result); strings.Contains(got, "DSML") || !strings.Contains(got, `"name":"run"`) || !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Fatalf("unexpected aggregate: %s", got)
	}
}

func TestCPAAsyncMarkupBridge(t *testing.T) {
	stream, body := markupStreamFixture(t)
	calls := recordStreamHostCalls(t, nil)
	setPumpHTTPClient(t, io.NopCloser(strings.NewReader(stream)))
	req, err := http.NewRequest(http.MethodPost, "https://upstream.invalid/chat", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	pumpUpstreamStream(req, nil, "markup-stream", false, "model", "model", "", time.Now(), "", "")
	var wire strings.Builder
	for _, call := range *calls {
		if call.method == pluginabi.MethodHostStreamEmit {
			var emit struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(call.body, &emit); err != nil {
				t.Fatal(err)
			}
			wire.Write(emit.Payload)
		}
	}
	if got := wire.String(); strings.Contains(got, "DSML") || !strings.Contains(got, `"name":"run"`) || !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Fatalf("unexpected async stream: %s", got)
	}
	if errors := nativeStreamErrors(t, *calls); len(errors) != 0 {
		t.Fatalf("stream errors: %v", errors)
	}
	if count := streamCloseCallCount(*calls); count != 1 {
		t.Fatalf("close count=%d", count)
	}
}

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
