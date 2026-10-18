package session

import "testing"

func TestCacheConversationIDStableAndSeparated(t *testing.T) {
	a := []byte(`{"messages":[{"role":"system","content":"system"},{"role":"user","content":"first"}]}`)
	later := []byte(`{"messages":[{"role":"system","content":"system"},{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"later"}]}`)
	b := []byte(`{"messages":[{"role":"system","content":"system"},{"role":"user","content":"different"}]}`)
	if CacheConversationID(a) == "" || CacheConversationID(a) != CacheConversationID(later) || CacheConversationID(a) == CacheConversationID(b) {
		t.Fatal("cache namespace not stable/separate")
	}
	if CacheConversationID([]byte(`{"conversation_id":"client"}`)) != "client" {
		t.Fatal("explicit conversation replaced")
	}
	if CacheConversationID([]byte(`{"messages":[]}`)) != "" {
		t.Fatal("invented empty conversation")
	}
}
