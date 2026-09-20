// usage_config.go decodes plugin config from config_yaml on every
// register/reconfigure call.
// All plugin-level config lives here so the rest of the plugin reads
// consistent, lock-protected snapshots. CPAMP usage-report resolution was
// removed with the usage forwarder feature.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// check-in schedule: 09:00 and 21:00 local time.
var checkinHours = []int{9, 21}

// plugin-level config decoded from plugin.register/reconfigure config_yaml.
var (
	checkinAuto   = true // enabled by default
	checkinAutoMu sync.RWMutex

	// managementAPIKey: plugin-layer auth for /v0/management/plugins/workbuddy/*
	// write endpoints. When empty, plugin relies on host-side auth (CPA's
	// management middleware) — that's the historical default and stays
	// backward-compatible. When set via config_yaml management_key: or env
	// WB_MANAGEMENT_KEY, handleManagement enforces constant-time Bearer match
	// plus per-IP token-bucket rate limiting on mutating endpoints.
	managementAPIKey   = ""
	managementAPIKeyMu sync.RWMutex
)

// configure decodes plugin config from the lifecycle request.
func configure(raw []byte) error {
	// Parse config without holding any lock (fixes nested-lock hazard).
	nextCheckinAuto := true
	nextLifecycleAuto := true
	nextSchedulerMode := schedulerModeOff // reset to default on reconfigure
	nextKeepaliveAuto := true
	nextMgmtKey := ""
	nextProxyURL := ""

	var configYAML []byte
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
			return errors.New("invalid plugin configuration")
		}
		configYAML = req.ConfigYAML
	}

	configScalars, err := parseTopLevelConfigScalars(configYAML)
	if err != nil {
		proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
		return err
	}
	if value, ok := configScalars["checkin_auto"]; ok {
		nextCheckinAuto = enabledConfigValue(value)
	}
	if value, ok := configScalars["lifecycle_auto"]; ok {
		nextLifecycleAuto = enabledConfigValue(value)
	}
	if configScalars["scheduler_mode"] == schedulerModeCredits {
		nextSchedulerMode = schedulerModeCredits
	}
	nextMgmtKey = configScalars["management_key"]
	if value, ok := configScalars["token_keepalive"]; ok {
		nextKeepaliveAuto = enabledConfigValue(value)
	}

	nextProxyURL, err = parseProxyURLConfig(configYAML)
	if err != nil {
		proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
		return err
	}
	nextFeatures, err := parseFeatureRuntime(configYAML)
	if err != nil {
		return err
	}
	if err := configureProxy(nextProxyURL); err != nil {
		return err
	}

	// Apply each setting under its own lock — no nesting.
	checkinAutoMu.Lock()
	checkinAuto = nextCheckinAuto
	checkinAutoMu.Unlock()

	lifecycleAutoMu.Lock()
	lifecycleAuto = nextLifecycleAuto
	lifecycleAutoMu.Unlock()

	schedulerModeMu.Lock()
	schedulerMode = nextSchedulerMode
	schedulerModeMu.Unlock()

	keepaliveAutoMu.Lock()
	keepaliveAuto = nextKeepaliveAuto
	keepaliveAutoMu.Unlock()

	// management key: config_yaml > env > keep existing. Empty stays empty
	// (plugin-layer auth disabled, host middleware still guards).
	if nextMgmtKey == "" {
		nextMgmtKey = strings.TrimSpace(os.Getenv("WB_MANAGEMENT_KEY"))
	}
	managementAPIKeyMu.Lock()
	managementAPIKey = nextMgmtKey
	managementAPIKeyMu.Unlock()

	ensureScheduler()
	currentModelRuntime().commitFeatureRuntime(nextFeatures)
	return nil
}

func parseValidatedConfigRoot(raw []byte) (*yaml.Node, error) {
	if strings.TrimSpace(string(raw)) == "" {
		return nil, nil
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("invalid config_yaml")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("config_yaml must contain exactly one document")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config_yaml must be a mapping")
	}
	root := document.Content[0]
	if err := validateConfigYAMLNode(root, strings.Split(string(raw), "\n")); err != nil {
		return nil, err
	}
	return root, nil
}

func validateConfigYAMLNode(node *yaml.Node, lines []string) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode || node.Alias != nil || node.Anchor != "" {
		return errors.New("config_yaml must not use anchors or aliases")
	}
	if node.Style&yaml.TaggedStyle != 0 || nodeStartsWithNonSpecificTag(node, lines) {
		return errors.New("config_yaml must not use explicit tags")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return errors.New("config_yaml mapping keys must be strings")
			}
			if key.Value == "<<" || key.Tag == "!!merge" {
				return errors.New("config_yaml must not use merge keys")
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return errors.New("config_yaml must not contain duplicate keys")
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := validateConfigYAMLNode(child, lines); err != nil {
			return err
		}
	}
	return nil
}

func nodeStartsWithNonSpecificTag(node *yaml.Node, lines []string) bool {
	if node.Line < 1 || node.Line > len(lines) || node.Column < 1 {
		return false
	}
	line := []rune(strings.TrimSuffix(lines[node.Line-1], "\r"))
	column := node.Column - 1
	if column >= len(line) || line[column] != '!' {
		return false
	}
	return column+1 == len(line) || line[column+1] == ' ' || line[column+1] == '\t'
}

func parseTopLevelConfigScalars(raw []byte) (map[string]string, error) {
	root, err := parseValidatedConfigRoot(raw)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, nil
	}
	values := make(map[string]string, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			continue
		}

		expected := ""
		switch key.Value {
		case "checkin_auto", "lifecycle_auto", "token_keepalive":
			expected = "boolean"
		case "scheduler_mode", "usage_report_url", "usage_report_key", "management_key":
			expected = "string"
		default:
			continue
		}
		if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
			continue
		}
		if value.Kind != yaml.ScalarNode {
			return nil, errors.New(key.Value + " must be a scalar " + expected)
		}
		if expected == "boolean" {
			if value.Tag != "!!bool" && value.Tag != "!!int" && value.Tag != "!!str" {
				return nil, errors.New(key.Value + " must be a boolean")
			}
		} else if value.Tag != "!!str" {
			return nil, errors.New(key.Value + " must be a string")
		}
		values[key.Value] = strings.TrimSpace(value.Value)
	}
	return values, nil
}

func enabledConfigValue(value string) bool {
	value = strings.ToLower(value)
	return value == "true" || value == "1" || value == "yes" || value == "on"
}

func parseProxyURLConfig(raw []byte) (string, error) {
	root, err := parseValidatedConfigRoot(raw)
	if err != nil {
		return "", err
	}
	if root == nil {
		return "", nil
	}
	value := ""
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "proxy-url" {
			continue
		}
		node := root.Content[i+1]
		if node.Kind != yaml.ScalarNode {
			return "", errors.New("proxy-url must be a string")
		}
		if node.Tag == "!!null" {
			value = ""
			continue
		}
		if node.Tag != "!!str" {
			return "", errors.New("proxy-url must be a string")
		}
		value = node.Value
	}
	return value, nil
}
