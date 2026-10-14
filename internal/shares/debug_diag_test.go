package shares

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
)

// TestDebugBrowserOps reproduces exactly the Realtime Database operations a
// signed-in browser performs for a specific pair of accounts. Temporary diagnostic.
func TestDebugBrowserOps(t *testing.T) {
	if os.Getenv("COINAPP_DEBUG") != "1" {
		t.Skip("diagnostic")
	}
	ctx := context.Background()
	sa := loadServiceAccount(t)
	dbURL := os.Getenv("FIREBASE_DATABASE_URL")
	apiKey := os.Getenv("FIREBASE_API_KEY")

	reader := os.Getenv("COINAPP_DEBUG_READER")
	owner := os.Getenv("COINAPP_DEBUG_OWNER")

	readerToken := customToken(t, ctx, sa, apiKey, reader)
	ownerToken := customToken(t, ctx, sa, apiKey, owner)

	do := func(who, method, path, token string, body any) {
		var readerBody io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			readerBody = bytes.NewReader(b)
		}
		target := dbURL + "/" + path + ".json?auth=" + token
		req, _ := http.NewRequest(method, target, readerBody)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		t.Logf("[%s] %-6s %-45s -> %d  %s", who, method, path, resp.StatusCode, string(payload))
	}

	// The reader's client: inbox, grants, then every owner it was shared with.
	do("reader", "GET", "inbox/"+reader, readerToken, nil)
	do("reader", "GET", "shares/"+reader, readerToken, nil)
	do("reader", "GET", "users/"+owner+"/profile", readerToken, nil)
	do("reader", "GET", "users/"+owner+"/owned", readerToken, nil)

	// The owner's client: its own collection, its inbox/outbox views, and the write
	// of its own profile that publishing does on sign-in.
	do("owner", "GET", "users/"+owner+"/owned", ownerToken, nil)
	do("owner", "GET", "inbox/"+owner, ownerToken, nil)
	do("owner", "GET", "shares/"+owner, ownerToken, nil)
	do("owner", "GET", "users/"+owner+"/profile", ownerToken, nil)
}
