package shares

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2/google"

	"coinapp-backend/internal/firebase"
)

// TestLiveShareFlow drives the real endpoints and, more importantly, the real
// security rules against the live Firebase project.
//
// The point of live sharing is that the recipient's browser reads the owner's
// collection directly, and a database rule decides whether it may. So this test
// proves the rule both ways: before acceptance the read must be denied, after
// acceptance it must succeed, and after removal it must be denied again. That is
// the part a unit test with a fake store cannot check.
//
// It is opt-in because it needs credentials and a real, existing recipient
// account:
//
//	COINAPP_LIVE=1 \
//	COINAPP_API_URL=http://localhost:8080 \
//	COINAPP_LIVE_RECIPIENT_EMAIL=someone@example.com \
//	FIREBASE_API_KEY=... FIREBASE_DATABASE_URL=... GOOGLE_APPLICATION_CREDENTIALS=... \
//	go test ./internal/shares/ -run TestLiveShareFlow -v
//
// The synthetic sender it creates is deleted again and every node it touched is
// cleared, so the project is left as it was found. The rules must be deployed
// first, or the "allowed" assertions below fail.
func TestLiveShareFlow(t *testing.T) {
	if os.Getenv("COINAPP_LIVE") != "1" {
		t.Skip("live test: set COINAPP_LIVE=1 and the variables documented in live_test.go")
	}

	apiURL := strings.TrimSuffix(os.Getenv("COINAPP_API_URL"), "/")
	dbURL := strings.TrimSuffix(os.Getenv("FIREBASE_DATABASE_URL"), "/")
	apiKey := os.Getenv("FIREBASE_API_KEY")
	recipientEmail := os.Getenv("COINAPP_LIVE_RECIPIENT_EMAIL")
	if apiURL == "" || dbURL == "" || apiKey == "" || recipientEmail == "" {
		t.Fatal("COINAPP_API_URL, FIREBASE_DATABASE_URL, FIREBASE_API_KEY and COINAPP_LIVE_RECIPIENT_EMAIL are required")
	}

	ctx := context.Background()
	sa := loadServiceAccount(t)

	client, err := firebase.New(ctx)
	if err != nil {
		t.Fatalf("firebase client: %v", err)
	}

	// A throwaway sender, so the test never depends on (or disturbs) a real user's
	// collection. Cleaned up whether the test passes or fails.
	senderUID := fmt.Sprintf("e2e-share-%d", time.Now().UnixNano())
	seed := map[string]int{"1": 3, "2": 1, "3": 5}
	if err := client.Put(ctx, "users/"+senderUID+"/owned", seed); err != nil {
		t.Fatalf("seeding the sender's collection: %v", err)
	}
	if err := client.Put(ctx, "users/"+senderUID+"/profile", map[string]string{
		"name": "E2E Sender", "email": "e2e-sender@example.com",
	}); err != nil {
		t.Fatalf("seeding the sender's profile: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Delete(context.Background(), "users/"+senderUID)
		_ = client.Delete(context.Background(), "outbox/"+senderUID)
		_ = deleteAccount(context.Background(), sa, senderUID)
	})

	recipientUID := lookupUID(t, ctx, sa, recipientEmail)
	senderToken := customToken(t, ctx, sa, apiKey, senderUID)
	recipientToken := customToken(t, ctx, sa, apiKey, recipientUID)

	// The recipient must not be able to see anything about the sender up front.
	if status, body := dbGet(t, dbURL, "users/"+senderUID+"/owned", recipientToken); status != http.StatusUnauthorized {
		t.Errorf("before any share, reading the sender's collection = %d, want 401 (body %s)", status, body)
	}

	// 1. Invite by email.
	var created struct {
		Recipients int      `json:"recipients"`
		Unknown    []string `json:"unknown"`
	}
	if status := call(t, http.MethodPost, apiURL+"/api/shares", senderToken, map[string]any{
		"recipients": []string{recipientEmail},
	}, &created); status != http.StatusCreated {
		t.Fatalf("create: status %d", status)
	}
	if created.Recipients != 1 {
		t.Errorf("recipients = %d, want 1", created.Recipients)
	}
	if len(created.Unknown) != 0 {
		t.Errorf("unknown = %v, want none", created.Unknown)
	}
	t.Cleanup(func() {
		call(t, http.MethodDelete, apiURL+"/api/shares/"+recipientUID, senderToken, nil, nil)
	})

	// 2. The invitation is in the recipient's own inbox, readable, and holds no
	//    snapshot.
	if status, body := dbGet(t, dbURL, "inbox/"+recipientUID+"/"+senderUID, recipientToken); status != http.StatusOK {
		t.Fatalf("reading own inbox = %d, want 200 (body %s)", status, body)
	} else if strings.Contains(string(body), "counts") {
		t.Errorf("the invitation carries counts; sharing must be a live read (body %s)", body)
	}

	// Still no access while it is only an invitation.
	if status, body := dbGet(t, dbURL, "users/"+senderUID+"/owned", recipientToken); status != http.StatusUnauthorized {
		t.Errorf("before accepting, reading the collection = %d, want 401 (body %s)", status, body)
	}

	// 3. Accept, then accept again: the second must not move the timestamp.
	var first struct {
		AcceptedAt int64 `json:"acceptedAt"`
	}
	if status := call(t, http.MethodPost, apiURL+"/api/shares/"+senderUID+"/accept", recipientToken, nil, &first); status != http.StatusOK {
		t.Fatalf("accept: status %d", status)
	}
	if first.AcceptedAt == 0 {
		t.Fatal("accept did not stamp acceptedAt")
	}
	var second struct {
		AcceptedAt int64 `json:"acceptedAt"`
	}
	if status := call(t, http.MethodPost, apiURL+"/api/shares/"+senderUID+"/accept", recipientToken, nil, &second); status != http.StatusOK {
		t.Fatalf("second accept: status %d", status)
	}
	if second.AcceptedAt != first.AcceptedAt {
		t.Errorf("a second accept moved the timestamp: %d -> %d", first.AcceptedAt, second.AcceptedAt)
	}

	// 4. THE CRUX: the rule now lets the recipient read the sender's live
	//    collection, and only in that direction.
	status, body := dbGet(t, dbURL, "users/"+senderUID+"/owned", recipientToken)
	if status != http.StatusOK {
		t.Fatalf("after accepting, reading the collection = %d, want 200 (body %s)", status, body)
	}
	// CoinCounts, not a plain map: the Realtime Database returns an all-integer
	// keyed node as an array with null gaps, which is exactly the shape here.
	var counts firebase.CoinCounts
	if err := json.Unmarshal(body, &counts); err != nil {
		t.Fatalf("decoding the sender's collection: %v (body %s)", err, body)
	}
	if len(counts) != len(seed) || counts["1"] != 3 || counts["3"] != 5 {
		t.Errorf("live collection = %v, want %v", counts, seed)
	}

	// The profile rule follows the same grant, so a share can name its owner.
	if status, body := dbGet(t, dbURL, "users/"+senderUID+"/profile", recipientToken); status != http.StatusOK {
		t.Errorf("after accepting, reading the profile = %d, want 200 (body %s)", status, body)
	}

	// Sharing is one-directional: the sender still cannot read the recipient.
	if status, body := dbGet(t, dbURL, "users/"+recipientUID+"/owned", senderToken); status != http.StatusUnauthorized {
		t.Errorf("the sender could read the recipient's collection = %d, want 401 (body %s)", status, body)
	}

	// 5. Remove it; access ends immediately.
	if status := call(t, http.MethodDelete, apiURL+"/api/shares/"+senderUID+"/inbox", recipientToken, nil, nil); status != http.StatusOK {
		t.Fatalf("remove: status %d", status)
	}
	if status, body := dbGet(t, dbURL, "users/"+senderUID+"/owned", recipientToken); status != http.StatusUnauthorized {
		t.Errorf("after removal, reading the collection = %d, want 401 (body %s)", status, body)
	}

	// 6. A forged token must be refused by the real verifier, not merely a missing
	//    one. This is the path that was silently broken before.
	if status := call(t, http.MethodPost, apiURL+"/api/shares/"+senderUID+"/accept", "not-a-real-token", nil, nil); status != http.StatusUnauthorized {
		t.Errorf("forged token: status %d, want 401", status)
	}
}

// --- live helpers ----------------------------------------------------------

// dbGet reads a Realtime Database path with an ID token, which is how the browser
// subscribes. A rule denial comes back as HTTP 401 "Permission denied".
func dbGet(t *testing.T, dbURL, path, token string) (int, []byte) {
	t.Helper()
	target := dbURL + "/" + strings.Trim(path, "/") + ".json?auth=" + url.QueryEscape(token)
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

type serviceAccount struct {
	ProjectID    string `json:"project_id"`
	ClientEmail  string `json:"client_email"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	Raw          []byte
}

func loadServiceAccount(t *testing.T) *serviceAccount {
	t.Helper()
	path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if path == "" {
		t.Fatal("GOOGLE_APPLICATION_CREDENTIALS is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading credentials: %v", err)
	}
	var sa serviceAccount
	if err := json.Unmarshal(raw, &sa); err != nil {
		t.Fatalf("parsing credentials: %v", err)
	}
	sa.Raw = raw
	return &sa
}

func adminToken(ctx context.Context, sa *serviceAccount) (string, error) {
	creds, err := google.CredentialsFromJSON(ctx, sa.Raw,
		"https://www.googleapis.com/auth/identitytoolkit",
		"https://www.googleapis.com/auth/userinfo.email")
	if err != nil {
		return "", err
	}
	token, err := creds.TokenSource.Token()
	if err != nil {
		return "", err
	}
	return token.AccessToken, nil
}

func lookupUID(t *testing.T, ctx context.Context, sa *serviceAccount, email string) string {
	t.Helper()
	token, err := adminToken(ctx, sa)
	if err != nil {
		t.Fatalf("admin token: %v", err)
	}
	target := "https://identitytoolkit.googleapis.com/v1/projects/" + sa.ProjectID + "/accounts:lookup"
	var out struct {
		Users []struct {
			LocalID string `json:"localId"`
		} `json:"users"`
	}
	if status := call(t, http.MethodPost, target, token, map[string][]string{"email": {email}}, &out); status != http.StatusOK {
		t.Fatalf("identity lookup: status %d", status)
	}
	if len(out.Users) == 0 {
		t.Fatalf("no CoinApp account exists for %s", email)
	}
	return out.Users[0].LocalID
}

func deleteAccount(ctx context.Context, sa *serviceAccount, uid string) error {
	token, err := adminToken(ctx, sa)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"localId": uid})
	target := "https://identitytoolkit.googleapis.com/v1/projects/" + sa.ProjectID + "/accounts:delete"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// customToken mints a Firebase custom token for a uid and trades it for an ID
// token, which is how this test authenticates without a browser.
func customToken(t *testing.T, ctx context.Context, sa *serviceAccount, apiKey, uid string) string {
	t.Helper()
	assertion, err := signCustomToken(sa, uid)
	if err != nil {
		t.Fatalf("signing a custom token: %v", err)
	}
	var out struct {
		IDToken string `json:"idToken"`
	}
	target := "https://identitytoolkit.googleapis.com/v1/accounts:signInWithCustomToken?key=" + apiKey
	if status := call(t, http.MethodPost, target, "", map[string]any{
		"token":             assertion,
		"returnSecureToken": true,
	}, &out); status != http.StatusOK || out.IDToken == "" {
		t.Fatalf("signInWithCustomToken: status %d", status)
	}
	return out.IDToken
}

func signCustomToken(sa *serviceAccount, uid string) (string, error) {
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", fmt.Errorf("private key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("private key is not RSA")
	}

	now := time.Now()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": sa.PrivateKeyID}
	claims := map[string]any{
		"iss": sa.ClientEmail,
		"sub": sa.ClientEmail,
		"aud": "https://identitytoolkit.googleapis.com/google.identity.identitytoolkit.v1.IdentityToolkit",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
		"uid": uid,
	}

	segment := func(value any) (string, error) {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(encoded), nil
	}
	head, err := segment(header)
	if err != nil {
		return "", err
	}
	body, err := segment(claims)
	if err != nil {
		return "", err
	}

	signing := head + "." + body
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func call(t *testing.T, method, target, token string, body any, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Logf("%s %s -> %d: %s", method, target, resp.StatusCode, payload)
	}
	if out != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			t.Fatalf("decoding %s %s: %v (body %s)", method, target, err, payload)
		}
	}
	return resp.StatusCode
}
