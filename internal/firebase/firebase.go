// Package firebase is a minimal Realtime Database client.
//
// The full google-api-go-client surface is far more than this needs: the catalog
// is one node read with GET and written with PUT, and the share endpoints need
// PATCH. So we exchange the service account JWT for an OAuth2 access token with
// golang.org/x/oauth2 and speak the REST API directly.
//
// Admin credentials bypass security rules entirely, which is why the catalog and
// the share nodes are only ever written from this process and never from a browser.
package firebase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// adminScopes is the OAuth2 scope set for Admin access.
//
// The Realtime Database REST endpoint does not accept a token minted for a
// single scope: a lone …/auth/firebase.database token is answered with
// 401 "Unauthorized request.", as is any token without the userinfo.email
// scope. Admin access requires userinfo.email alongside at least one other
// scope, which is why the official Admin SDK requests a whole set. These two
// are the smallest pair that works, verified against the live database.
var adminScopes = []string{
	"https://www.googleapis.com/auth/firebase.database",
	"https://www.googleapis.com/auth/userinfo.email",
}

// identityScopes is the scope set for the Admin Identity Toolkit, used to turn a
// recipient's email into a uid. It is deliberately separate from adminScopes: the
// Realtime Database endpoint is picky about the exact scope set it accepts, so the
// database token is left untouched.
var identityScopes = []string{
	"https://www.googleapis.com/auth/identitytoolkit",
	"https://www.googleapis.com/auth/userinfo.email",
}

type Client struct {
	base     string
	http     *http.Client
	tokens   oauth2.TokenSource
	identity oauth2.TokenSource
	project  string
	readOnly bool
}

// Meta is the small header the web app reads to decide whether its cached catalog
// is stale, without pulling the whole 450 KB payload.
type Meta struct {
	Count    int   `json:"count"`
	Total    int   `json:"total"`
	Schema   int   `json:"schema"`
	SyncedAt int64 `json:"syncedAt"`
	LastSync struct {
		Added     int `json:"added"`
		Updated   int `json:"updated"`
		Unchanged int `json:"unchanged"`
		Rematched int `json:"rematched"`
		Removed   int `json:"removed"`
	} `json:"lastSync"`
}

// New builds a client from the environment.
//
// Credentials are never read from a file inside this repository:
//
//	GOOGLE_APPLICATION_CREDENTIALS=/etc/coinapp/service-account.json
//	FIREBASE_SERVICE_ACCOUNT='{"type":"service_account","private_key":"..."}'
//	FIREBASE_DATABASE_URL=https://coinapp-a454b.firebaseio.com
//
// With none set, Application Default Credentials are used (gcloud auth).
func New(ctx context.Context) (*Client, error) {
	raw, err := serviceAccountJSON()
	if err != nil {
		return nil, err
	}

	databaseURL := os.Getenv("FIREBASE_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "https://coinapp-a454b.firebaseio.com"
	}
	if !strings.HasPrefix(databaseURL, "https://") {
		return nil, fmt.Errorf("FIREBASE_DATABASE_URL must be https, got %q", databaseURL)
	}

	credentials, err := google.CredentialsFromJSON(ctx, raw, adminScopes...)
	if err != nil {
		return nil, fmt.Errorf("reading service account credentials: %w", err)
	}

	identity, err := google.CredentialsFromJSON(ctx, raw, identityScopes...)
	if err != nil {
		return nil, fmt.Errorf("reading identity credentials: %w", err)
	}

	return &Client{
		base:     strings.TrimSuffix(databaseURL, "/"),
		http:     &http.Client{Timeout: 30 * time.Second},
		tokens:   credentials.TokenSource,
		identity: identity.TokenSource,
		project:  credentials.ProjectID,
	}, nil
}

func serviceAccountJSON() ([]byte, error) {
	if inline := os.Getenv("FIREBASE_SERVICE_ACCOUNT"); inline != "" {
		return []byte(inline), nil
	}
	path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if path == "" {
		return nil, fmt.Errorf("no credentials: set GOOGLE_APPLICATION_CREDENTIALS or FIREBASE_SERVICE_ACCOUNT")
	}
	return os.ReadFile(path)
}

// HasCredentials reports whether an Admin credential is configured.
func HasCredentials() bool {
	return os.Getenv("FIREBASE_SERVICE_ACCOUNT") != "" || os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != ""
}

// NewReadOnly builds a client that can only issue unauthenticated GETs.
//
// The catalog node is world-readable, which lets you preview a migration and see
// exactly what a sync would change with no credentials at all. Any write attempt
// returns an error rather than silently failing against the security rules.
func NewReadOnly() *Client {
	databaseURL := os.Getenv("FIREBASE_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "https://coinapp-a454b.firebaseio.com"
	}
	return &Client{
		base:     strings.TrimSuffix(databaseURL, "/"),
		http:     &http.Client{Timeout: 30 * time.Second},
		tokens:   denyAllTokens{},
		readOnly: true,
	}
}

type denyAllTokens struct{}

func (denyAllTokens) Token() (*oauth2.Token, error) {
	return nil, errors.New("this client is read-only: Admin credentials are required to write")
}

// endpoint maps a database path to its REST URL.
//
// RTDB forbids the characters . # $ [ ] / in keys. Rejecting them here turns a
// confusing 403 from Firebase into an obvious local error.
func (c *Client) endpoint(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty database path")
	}
	for _, segment := range strings.Split(strings.Trim(path, "/"), "/") {
		if strings.ContainsAny(segment, ".#$[]/") {
			return "", fmt.Errorf("illegal character in database path segment %q", segment)
		}
	}
	return c.base + "/" + strings.Trim(path, "/") + ".json", nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	target, err := c.endpoint(path)
	if err != nil {
		return err
	}

	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if !c.readOnly {
		token, err := c.tokens.Token()
		if err != nil {
			return err
		}
		token.SetAuthHeader(req)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s -> %d: %s", method, path, resp.StatusCode, truncate(string(payload), 300))
	}

	if out != nil && len(payload) > 0 && string(payload) != "null" {
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("decoding %s: %w", path, err)
		}
	}
	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) Put(ctx context.Context, path string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, path, body, nil)
}

// Patch merges children into a node without replacing it.
//
// value must marshal to a JSON object: the Realtime Database rejects a PATCH
// whose body is not an object, so a single leaf is set by patching its parent
// (for example {"acceptedAt": now} rather than a bare number).
func (c *Client) Patch(ctx context.Context, path string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, path, body, nil)
}

func (c *Client) Delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// ReadCatalogRaw returns the catalog node exactly as stored, without interpreting
// its shape. The catalog has existed both as a positional array and as a keyed map,
// so deciding how to read it belongs to the catalog package, not here.
func (c *Client) ReadCatalogRaw(ctx context.Context) ([]byte, error) {
	var raw json.RawMessage
	if err := c.Get(ctx, "coins", &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *Client) ReadMeta(ctx context.Context) (*Meta, error) {
	var meta Meta
	if err := c.Get(ctx, "meta", &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// WriteCatalog swaps the catalog in a single PUT, so a client never observes a
// half-updated catalog. Callers skip the write entirely when nothing changed.
func (c *Client) WriteCatalog(ctx context.Context, coins any, meta Meta) error {
	if err := c.Put(ctx, "coins", coins); err != nil {
		return err
	}
	return c.Put(ctx, "meta", meta)
}

// ReadUserOwned returns one user's counts.
func (c *Client) ReadUserOwned(ctx context.Context, uid string) (map[string]int, error) {
	var owned CoinCounts
	if err := c.Get(ctx, "users/"+url.PathEscape(uid)+"/owned", &owned); err != nil {
		return nil, err
	}
	return owned, nil
}

// CoinCounts maps a coin id to a held count.
//
// It exists because the Realtime Database coerces a node whose keys are all
// integers into a JSON array, padding the gaps with nulls. Coin ids are often
// integers (the legacy "0".."501"), so a counts node can come back either as an
// object or as an array, and decoding has to accept both or a perfectly valid
// collection fails to read. A null slot — an array gap — is simply dropped.
type CoinCounts map[string]int

func (c *CoinCounts) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*c = nil
		return nil
	}

	if trimmed[0] == '[' {
		var list []*int
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		counts := make(CoinCounts, len(list))
		for i, count := range list {
			if count != nil {
				counts[strconv.Itoa(i)] = *count
			}
		}
		*c = counts
		return nil
	}

	var counts map[string]int
	if err := json.Unmarshal(trimmed, &counts); err != nil {
		return err
	}
	*c = counts
	return nil
}

// UIDByEmail resolves an account's email to its Firebase uid, via the Admin
// Identity Toolkit (not the Realtime Database REST API, which has no such lookup).
//
// Returns an empty uid and no error when no account has that email, so callers can
// report "unknown recipient" rather than a failure. This is what lets a share be
// addressed to what a person actually knows — an email — while the inbox stays
// keyed by uid.
func (c *Client) UIDByEmail(ctx context.Context, email string) (string, error) {
	if c.readOnly || c.identity == nil {
		return "", errors.New("email lookup needs Admin credentials")
	}
	if c.project == "" {
		return "", errors.New("service account has no project_id")
	}

	body, err := json.Marshal(map[string][]string{"email": {email}})
	if err != nil {
		return "", err
	}

	target := "https://identitytoolkit.googleapis.com/v1/projects/" + c.project + "/accounts:lookup"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	token, err := c.identity.Token()
	if err != nil {
		return "", err
	}
	token.SetAuthHeader(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("identity lookup %q -> %d: %s", email, resp.StatusCode, truncate(string(payload), 300))
	}

	var out struct {
		Users []struct {
			LocalID string `json:"localId"`
		} `json:"users"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", fmt.Errorf("decoding identity lookup for %q: %w", email, err)
	}
	if len(out.Users) == 0 {
		return "", nil
	}
	return out.Users[0].LocalID, nil
}
