package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegistrationConfigFieldsMatchImplementedConfig(t *testing.T) {
	fields := wbRegistration().Metadata.ConfigFields
	managementKeyCount := 0
	for _, field := range fields {
		switch field.Name {
		case "management_key":
			managementKeyCount++
		case "proxy-url", "proxy_url", "usage_report_url", "usage_report_key":
			t.Fatalf("removed config field %q must not be registered", field.Name)
		case "scheduler_mode":
			if strings.Contains(strings.ToLower(field.Description), "highest remaining") {
				t.Fatal("scheduler_mode description advertises unimplemented highest-credit ranking")
			}
			if !strings.Contains(strings.ToLower(field.Description), "panel-selected") {
				t.Fatal("scheduler_mode description must document panel-selected routing")
			}
		}
	}
	if managementKeyCount != 1 {
		t.Fatalf("management_key config field count = %d", managementKeyCount)
	}
}

func TestRegistrationExposesForkFusionConfig(t *testing.T) {
	got := make(map[string]pluginapi.ConfigField)
	for _, field := range wbRegistration().Metadata.ConfigFields {
		got[field.Name] = field
	}
	for name, wantType := range map[string]pluginapi.ConfigFieldType{
		"desensitize":        pluginapi.ConfigFieldTypeBoolean,
		"desensitize_terms":  pluginapi.ConfigFieldTypeArray,
		"enterprise_credits": pluginapi.ConfigFieldTypeBoolean,
	} {
		field, ok := got[name]
		if !ok {
			t.Fatalf("missing %s config field", name)
		}
		if field.Type != wantType {
			t.Fatalf("%s type = %q, want %q", name, field.Type, wantType)
		}
	}
	mode, ok := got["oauth_client_mode"]
	if !ok {
		t.Fatal("missing oauth_client_mode config field")
	}
	if mode.Type != pluginapi.ConfigFieldTypeEnum || !sameStrings(mode.EnumValues, []string{"cli", "workbuddy"}) {
		t.Fatalf("oauth_client_mode = %#v", mode)
	}
}

func TestRegistrationDocumentsConfiguredModelsContract(t *testing.T) {
	count := 0
	var models pluginapi.ConfigField
	for _, field := range wbRegistration().Metadata.ConfigFields {
		if field.Name == "models" {
			count++
			models = field
		}
	}
	if count != 1 {
		t.Fatalf("models config field count = %d, want 1", count)
	}
	if models.Type != pluginapi.ConfigFieldTypeArray {
		t.Fatalf("models config field type = %q, want array", models.Type)
	}
	description := strings.ToLower(models.Description)
	for _, required := range []string{"strings only", "single-line", "non-empty", "complete", "bypasses workbuddy", "http", "cache", "models.dev", "metadata", "missing", "null", "[]"} {
		if !strings.Contains(description, required) {
			t.Errorf("models description missing %q: %q", required, models.Description)
		}
	}
}
