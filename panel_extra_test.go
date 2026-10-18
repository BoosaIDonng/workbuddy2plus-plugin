package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAuthStateKeyPrefersHostID(t *testing.T) {
	entry := pluginapi.HostAuthFileEntry{ID: "host-id", AuthIndex: "auth-index"}
	if got := authStateKey(entry); got != "host-id" {
		t.Fatalf("authStateKey = %q want host-id", got)
	}
	entry.ID = ""
	if got := authStateKey(entry); got != "auth-index" {
		t.Fatalf("authStateKey fallback = %q want auth-index", got)
	}
}
