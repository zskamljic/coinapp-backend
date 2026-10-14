package shares

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeStore is an in-memory stand-in for *firebase.Client. It mirrors the parts of
// the Realtime Database the API depends on: Get/Put/Delete address a path of
// slash-separated segments, and Put replaces a node.
type fakeStore struct {
	root   map[string]any
	emails map[string]string // email -> uid
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		root:   map[string]any{},
		emails: map[string]string{},
	}
}

func segments(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func (f *fakeStore) node(path string) (any, bool) {
	var current any = f.root
	for _, seg := range segments(path) {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func (f *fakeStore) set(path string, value any) {
	segs := segments(path)
	if len(segs) == 0 {
		if m, ok := value.(map[string]any); ok {
			f.root = m
		}
		return
	}
	current := f.root
	for _, seg := range segs[:len(segs)-1] {
		child, ok := current[seg].(map[string]any)
		if !ok {
			child = map[string]any{}
			current[seg] = child
		}
		current = child
	}
	current[segs[len(segs)-1]] = value
}

func (f *fakeStore) remove(path string) {
	segs := segments(path)
	if len(segs) == 0 {
		return
	}
	current := f.root
	for _, seg := range segs[:len(segs)-1] {
		child, ok := current[seg].(map[string]any)
		if !ok {
			return
		}
		current = child
	}
	delete(current, segs[len(segs)-1])
}

func toAny(value any) any {
	b, _ := json.Marshal(value)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func (f *fakeStore) Get(_ context.Context, path string, out any) error {
	node, ok := f.node(path)
	if !ok || node == nil {
		return nil // a missing node reads back as the zero value, like RTDB
	}
	b, err := json.Marshal(node)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (f *fakeStore) Put(_ context.Context, path string, value any) error {
	f.set(path, toAny(value))
	return nil
}

func (f *fakeStore) Delete(_ context.Context, path string) error {
	f.remove(path)
	return nil
}

func (f *fakeStore) UIDByEmail(_ context.Context, email string) (string, error) {
	return f.emails[email], nil
}

// --- helpers ---------------------------------------------------------------

type endpoint func(http.ResponseWriter, *http.Request, string)

func invoke(h endpoint, uid, method, target, body string, pathValues map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req, uid)
	return rec
}

func newAPI(store *fakeStore) *API {
	return &API{db: store, apiKey: "test-key"}
}

func exists(store *fakeStore, path string) bool {
	_, ok := store.node(path)
	return ok
}

// invite seeds one pending invitation from sender to a recipient's uid.
func invite(t *testing.T, api *API, store *fakeStore, sender, recipientUID, recipientEmail string) {
	t.Helper()
	store.emails[recipientEmail] = recipientUID
	rec := invoke(api.createShare, sender, http.MethodPost, "/api/shares",
		`{"recipients":["`+recipientEmail+`"]}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed createShare: got %d, body %s", rec.Code, rec.Body.String())
	}
}

// --- createShare -----------------------------------------------------------

func TestCreateShareWritesOnlyPointers(t *testing.T) {
	store := newFakeStore()
	store.emails["maja@example.com"] = "maja-uid"
	store.emails["sender@example.com"] = "sender-uid"

	api := newAPI(store)
	rec := invoke(api.createShare, "sender-uid", http.MethodPost, "/api/shares",
		`{"recipients":["Maja@Example.com","ghost@example.com","sender@example.com"]}`, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, body %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Recipients int      `json:"recipients"`
		Unknown    []string `json:"unknown"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if out.Recipients != 1 {
		t.Errorf("recipients = %d, want 1", out.Recipients)
	}
	if len(out.Unknown) != 2 || out.Unknown[0] != "ghost@example.com" || out.Unknown[1] != "sender@example.com" {
		t.Errorf("unknown = %v, want [ghost@example.com sender@example.com] (self is not a recipient)", out.Unknown)
	}

	// The invitation is a pointer, not a copy: exactly one node, carrying only a
	// timestamp. No counts, no name, no title.
	raw, ok := store.node("inbox/maja-uid/sender-uid")
	if !ok {
		t.Fatal("no invitation was written to the recipient's inbox")
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("invitation is %T, want an object", raw)
	}
	if len(fields) != 1 {
		t.Errorf("invitation = %v, want a single createdAt field", fields)
	}
	if _, present := fields["createdAt"]; !present {
		t.Errorf("invitation = %v, want a createdAt field", fields)
	}
	if _, leaked := fields["counts"]; leaked {
		t.Error("the invitation carries a snapshot of counts; sharing must be a live read")
	}

	// The owner keeps their own record so they can revoke later.
	if !exists(store, "outbox/sender-uid/maja-uid") {
		t.Error("no outbox record was written, so the owner cannot revoke")
	}
}

func TestCreateShareRejectsBadRequests(t *testing.T) {
	store := newFakeStore()
	store.emails["maja@example.com"] = "maja-uid"
	api := newAPI(store)

	cases := []struct {
		name string
		body string
	}{
		{"no recipients", `{"recipients":[]}`},
		{"nobody known", `{"recipients":["ghost@example.com"]}`},
		{"not an email", `{"recipients":["not-an-email"]}`},
		{"only self", `{"recipients":["sender@example.com"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store.emails["sender@example.com"] = "sender-uid"
			rec := invoke(api.createShare, "sender-uid", http.MethodPost, "/api/shares", tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("got %d, want 400; body %s", rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("invalid JSON", func(t *testing.T) {
		rec := invoke(api.createShare, "sender-uid", http.MethodPost, "/api/shares", `{`, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d, want 400", rec.Code)
		}
	})

	t.Run("too many recipients", func(t *testing.T) {
		var emails []string
		for i := 0; i <= maxRecipients; i++ {
			emails = append(emails, fmt.Sprintf("person%d@example.com", i))
		}
		body, _ := json.Marshal(map[string]any{"recipients": emails})
		rec := invoke(api.createShare, "sender-uid", http.MethodPost, "/api/shares", string(body), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d, want 400", rec.Code)
		}
	})
}

func TestCreateShareDoesNotReinviteAnAcceptedShare(t *testing.T) {
	store := newFakeStore()
	api := newAPI(store)
	invite(t, api, store, "sender-uid", "maja-uid", "maja@example.com")

	invoke(api.acceptShare, "maja-uid", http.MethodPost, "/api/shares/sender-uid/accept", "",
		map[string]string{"uid": "sender-uid"})

	// Share again with the same person: it must not resurrect a pending invite.
	rec := invoke(api.createShare, "sender-uid", http.MethodPost, "/api/shares",
		`{"recipients":["maja@example.com"]}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-share: got %d, body %s", rec.Code, rec.Body.String())
	}
	if exists(store, "inbox/maja-uid/sender-uid") {
		t.Error("re-sharing an accepted share left a pending invitation behind")
	}
}

// --- acceptShare -----------------------------------------------------------

func TestAcceptWritesTheGrantUnderTheReaderAndIsIdempotent(t *testing.T) {
	store := newFakeStore()
	api := newAPI(store)
	invite(t, api, store, "sender-uid", "maja-uid", "maja@example.com")
	if !exists(store, "inbox/maja-uid/sender-uid") {
		t.Fatal("setup: no pending invitation")
	}

	first := invoke(api.acceptShare, "maja-uid", http.MethodPost, "/api/shares/sender-uid/accept", "",
		map[string]string{"uid": "sender-uid"})
	if first.Code != http.StatusOK {
		t.Fatalf("accept: got %d, body %s", first.Code, first.Body.String())
	}
	var once struct {
		AcceptedAt int64 `json:"acceptedAt"`
	}
	json.Unmarshal(first.Body.Bytes(), &once)
	if once.AcceptedAt == 0 {
		t.Fatal("accept did not stamp acceptedAt")
	}

	// This is the node the security rules consult. It must be keyed by the READER
	// and name the OWNER — the opposite would hand the reader somebody else's list.
	if !exists(store, "shares/maja-uid/sender-uid") {
		t.Fatal("accept did not write the grant under shares/{reader}/{owner}")
	}
	if exists(store, "shares/sender-uid/maja-uid") {
		t.Error("accept wrote the grant backwards")
	}
	if exists(store, "inbox/maja-uid/sender-uid") {
		t.Error("the pending invitation survived acceptance")
	}

	// Accepting again changes nothing.
	second := invoke(api.acceptShare, "maja-uid", http.MethodPost, "/api/shares/sender-uid/accept", "",
		map[string]string{"uid": "sender-uid"})
	if second.Code != http.StatusOK {
		t.Fatalf("second accept: got %d, body %s", second.Code, second.Body.String())
	}
	var twice struct {
		AcceptedAt int64 `json:"acceptedAt"`
	}
	json.Unmarshal(second.Body.Bytes(), &twice)
	if twice.AcceptedAt != once.AcceptedAt {
		t.Errorf("a second accept moved the timestamp: %d -> %d", once.AcceptedAt, twice.AcceptedAt)
	}
}

func TestAcceptUnknownInviteIs404(t *testing.T) {
	api := newAPI(newFakeStore())
	rec := invoke(api.acceptShare, "maja-uid", http.MethodPost, "/api/shares/nobody/accept", "",
		map[string]string{"uid": "nobody"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", rec.Code)
	}
}

// --- removeShare -----------------------------------------------------------

func TestRemoveDropsAPendingInvitation(t *testing.T) {
	store := newFakeStore()
	api := newAPI(store)
	invite(t, api, store, "sender-uid", "maja-uid", "maja@example.com")

	rec := invoke(api.removeShare, "maja-uid", http.MethodDelete, "/api/shares/sender-uid/inbox", "",
		map[string]string{"uid": "sender-uid"})
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: got %d, body %s", rec.Code, rec.Body.String())
	}
	if exists(store, "inbox/maja-uid/sender-uid") {
		t.Error("the rejected invitation is still present")
	}
	if exists(store, "outbox/sender-uid/maja-uid") {
		t.Error("the owner still believes the share is outstanding")
	}
}

func TestRemoveEndsAnAcceptedShare(t *testing.T) {
	store := newFakeStore()
	api := newAPI(store)
	invite(t, api, store, "sender-uid", "maja-uid", "maja@example.com")
	invoke(api.acceptShare, "maja-uid", http.MethodPost, "/api/shares/sender-uid/accept", "",
		map[string]string{"uid": "sender-uid"})

	if !exists(store, "shares/maja-uid/sender-uid") {
		t.Fatal("setup: no grant")
	}

	rec := invoke(api.removeShare, "maja-uid", http.MethodDelete, "/api/shares/sender-uid/inbox", "",
		map[string]string{"uid": "sender-uid"})
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: got %d, body %s", rec.Code, rec.Body.String())
	}
	// Losing the grant is what actually revokes the reader's access.
	if exists(store, "shares/maja-uid/sender-uid") {
		t.Error("the grant survived removal, so access was not revoked")
	}
	if exists(store, "outbox/sender-uid/maja-uid") {
		t.Error("the owner still believes the share is outstanding")
	}
}

// --- revokeShare -----------------------------------------------------------

func TestRevokeClearsGrantInviteAndOwnerRecord(t *testing.T) {
	store := newFakeStore()
	store.emails["maja@example.com"] = "maja-uid"
	store.emails["uros@example.com"] = "uros-uid"

	api := newAPI(store)
	rec := invoke(api.createShare, "sender-uid", http.MethodPost, "/api/shares",
		`{"recipients":["maja@example.com","uros@example.com"]}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, body %s", rec.Code, rec.Body.String())
	}

	// One accepts; the other never opens the app. Revoke must clear both.
	invoke(api.acceptShare, "maja-uid", http.MethodPost, "/api/shares/sender-uid/accept", "",
		map[string]string{"uid": "sender-uid"})

	revoked := invoke(api.revokeShare, "sender-uid", http.MethodDelete, "/api/shares/maja-uid", "",
		map[string]string{"uid": "maja-uid"})
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke: got %d, body %s", revoked.Code, revoked.Body.String())
	}
	if exists(store, "shares/maja-uid/sender-uid") {
		t.Error("revoke left the grant, so the recipient can still read the collection")
	}
	if exists(store, "outbox/sender-uid/maja-uid") {
		t.Error("revoke left the owner's record")
	}

	// The pending recipient is untouched until the owner revokes them too.
	if !exists(store, "inbox/uros-uid/sender-uid") {
		t.Error("revoking one reader dropped another reader's invitation")
	}
	invoke(api.revokeShare, "sender-uid", http.MethodDelete, "/api/shares/uros-uid", "",
		map[string]string{"uid": "uros-uid"})
	if exists(store, "inbox/uros-uid/sender-uid") {
		t.Error("revoke left the pending recipient's invitation")
	}
}

// --- auth ------------------------------------------------------------------

func TestAuthenticatedRequiresBearerToken(t *testing.T) {
	api := newAPI(newFakeStore())
	handler := api.authenticated(api.acceptShare)

	req := httptest.NewRequest(http.MethodPost, "/api/shares/x/accept", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", rec.Code)
	}
}

func TestAuthenticatedWithoutAPIKeyIsUnavailable(t *testing.T) {
	api := &API{db: newFakeStore()}
	handler := api.authenticated(api.acceptShare)

	req := httptest.NewRequest(http.MethodPost, "/api/shares/x/accept", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", rec.Code)
	}
}
