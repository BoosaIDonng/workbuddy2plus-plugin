// panel_extra.go adds the panel's model-center and account admin endpoints.
//
// Model center (A5): exposes the full upstream model catalog — display name,
// context window, max output, reasoning tiers, credit multiplier, tags —
// which CPA's own /v1/models strips down to id/object/owned_by.
//
// Account admin (A6): temporary disable / re-enable, the "pull this account
// out of rotation without deleting its credentials" operation. Writes go
// through host.auth.save like every other credential mutation.
package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// appendNote appends a short marker to a display note (bounded length).
func appendNote(note, extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" || strings.Contains(note, extra) {
		return note
	}
	if len(note)+len(extra) >= 75 {
		return note
	}
	if note == "" {
		return extra
	}
	return note + " · " + extra
}

// modelCenterRow is one model in the panel's catalog view.
type modelCenterRow struct {
	ID             string   `json:"id"`
	Name           string   `json:"name,omitempty"`
	Vendor         string   `json:"vendor,omitempty"`
	Description    string   `json:"description,omitempty"`
	ContextWindow  int64    `json:"context_window,omitempty"`
	MaxTokens      int64    `json:"max_tokens,omitempty"`
	Credits        string   `json:"credits,omitempty"`
	Efforts        []string `json:"efforts,omitempty"`
	DefaultEffort  string   `json:"default_effort,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	SupportsImages bool     `json:"supports_images,omitempty"`
	SupportsTools  bool     `json:"supports_tools,omitempty"`
	SupportsReason bool     `json:"supports_reasoning,omitempty"`
	IsDefault      bool     `json:"is_default,omitempty"`
	Realm          string   `json:"realm,omitempty"`
}

// handleModelCenter serves GET /models — the full model catalog per account,
// deduplicated by model id (first account that advertises it wins; realm is
// recorded so the panel can show CN vs Global availability).
func handleModelCenter(req pluginapi.ManagementRequest) (int, any) {
	files, err := panelHostAuthList()
	if err != nil {
		return http.StatusBadGateway, map[string]any{"error": "auth list unavailable"}
	}
	seen := map[string]bool{}
	var rows []modelCenterRow
	var errs []string
	for _, f := range files {
		if f.Disabled {
			continue
		}
		sa, _, err := hostAuthGetBundle(f.AuthIndex)
		if err != nil {
			errs = append(errs, shortName(f)+": load auth failed")
			continue
		}
		a := wbAuth(nil, sa)
		infos, err := wbClient.FetchModels(a)
		if err != nil {
			errs = append(errs, shortName(f)+": "+sanitizeUpstreamError(err))
			continue
		}
		realm := a.Realm()
		for _, m := range infos {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			rows = append(rows, modelCenterRow{
				ID:             m.ID,
				Name:           m.Name,
				Vendor:         m.Vendor,
				Description:    m.Description,
				ContextWindow:  m.ContextWindow,
				MaxTokens:      m.MaxTokens,
				Credits:        m.Credits,
				Efforts:        m.Efforts,
				DefaultEffort:  m.DefaultEffort,
				Tags:           m.Tags,
				SupportsImages: m.SupportsImages,
				SupportsTools:  m.SupportsToolCall,
				SupportsReason: m.SupportsReasoning,
				IsDefault:      m.IsDefault,
				Realm:          realm,
			})
		}
	}
	out := map[string]any{"models": rows, "count": len(rows)}
	if len(errs) > 0 {
		out["errors"] = errs
	}
	return http.StatusOK, out
}

// handleAccountToggle serves POST /account/toggle {auth_index, disabled} —
// pull an account out of rotation (or put it back) without deleting it: the
// "this account is dragging the pool down, shelf it for now" operation.
// Writes go through the same auth-file pipeline the lifecycle uses, so extra
// fields in the physical file are preserved.
func handleAccountToggle(req pluginapi.ManagementRequest) (int, any) {
	var body struct {
		AuthIndex string `json:"auth_index"`
		Disabled  *bool  `json:"disabled"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return http.StatusBadRequest, map[string]any{"error": "invalid request body"}
	}
	idx := strings.TrimSpace(body.AuthIndex)
	if idx == "" || len(idx) > 128 {
		return http.StatusBadRequest, map[string]any{"error": "invalid auth_index"}
	}
	if body.Disabled == nil {
		return http.StatusBadRequest, map[string]any{"error": "disabled is required"}
	}
	sa, _, err := hostAuthGetBundle(idx)
	if err != nil {
		return http.StatusNotFound, map[string]any{"error": "account not found"}
	}
	// Manual state carries an explicit note so it is distinguishable from the
	// automatic credit-driven disable in the auth file and the panel.
	note := displayNote(sa, nil, *body.Disabled)
	if *body.Disabled {
		note = appendNote(note, "手动停用")
	}
	if err := writeAuthDisabled(idx, sa, *body.Disabled, note); err != nil {
		return http.StatusBadGateway, map[string]any{"error": sanitizeUpstreamError(err)}
	}
	action := "enabled"
	if *body.Disabled {
		action = "disabled"
	}
	recordTask("account-"+action, sa.Account.Nickname, true, "by panel")
	return http.StatusOK, map[string]any{
		"auth_index": idx,
		"nickname":   sa.Account.Nickname,
		"disabled":   *body.Disabled,
	}
}

// writeAuthDisabled persists the disabled flag plus note for one account,
// reusing the lifecycle's file pipeline (extra fields preserved).
func writeAuthDisabled(authIndex string, sa *storedAuth, disabled bool, note string) error {
	mu := checkinLockFor(authIndex)
	mu.Lock()
	defer mu.Unlock()

	phys, err := hostAuthGetPhysical(authIndex)
	name := authFileNameFor(sa)
	path := ""
	legacyPath := ""
	if err == nil && phys != nil {
		name, path, legacyPath = resolveAuthFileTarget(sa, phys)
	}
	raw, err := buildAuthFileJSON(sa, disabled, note, nil)
	if err != nil {
		return err
	}
	if err := hostAuthPersistMigrate(name, path, legacyPath, raw); err != nil {
		return err
	}
	rememberLifecycleState(authIndex, disabled, note)
	accountCache.Delete(authIndex)
	return nil
}
