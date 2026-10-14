// Package shares serves the share mechanic.
//
// Sharing is a live read, not a copy. There is no snapshot of anybody's counts
// anywhere: the recipient's browser subscribes directly to the owner's
// /users/{owner}/owned, so what they see is always the owner's current
// collection. The backend's whole job is to own the access-control list, because
// no client may write it — that is what stops a user from adding themselves to
// somebody else's collection.
//
// The data model:
//
//	/shares/{readerUid}/{ownerUid}   the grant: reader may read owner's /owned
//	/inbox/{readerUid}/{ownerUid}    a pending invitation, not yet accepted
//	/outbox/{ownerUid}/{readerUid}   the owner's own record, so they can revoke
//
// Only the backend writes those three nodes (Admin credentials bypass the rules).
// The security rules turn the grant into actual read access:
//
//	users/$uid/owned   .read: root.child('shares').child(auth.uid).child($uid).exists()
//
// so "you can read somebody's collection only if they shared it with you" is
// enforced by the database, not by this process promising to be careful.
//
// Recipients are addressed by email, because that is what a person knows; every
// node is keyed by uid, so an email is resolved to a uid before delivery.
package shares

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"coinapp-backend/internal/firebase"
)

// maxRecipients bounds one share so a single request cannot fan out unbounded.
const maxRecipients = 50

// Store is the subset of the database client this API needs. It is an interface so
// the handlers can be tested against an in-memory fake with no live Firebase.
// *firebase.Client satisfies it.
type Store interface {
	Get(ctx context.Context, path string, out any) error
	Put(ctx context.Context, path string, value any) error
	Delete(ctx context.Context, path string) error
	UIDByEmail(ctx context.Context, email string) (string, error)
}

// Relation is the shape of every share node.
//
// An inbox or outbox entry carries CreatedAt; a grant carries AcceptedAt. One
// small struct serves all three, and the zero value is what a missing node reads
// back as, so a single check covers "does this relationship exist".
type Relation struct {
	CreatedAt  int64 `json:"createdAt,omitempty"`
	AcceptedAt int64 `json:"acceptedAt,omitempty"`
}

// API is the handler's dependency set.
type API struct {
	db        Store
	apiKey    string
	allowList map[string]struct{}
}

func Handler(store Store, allowOrigins []string) http.Handler {
	api := &API{
		db:        store,
		apiKey:    os.Getenv("FIREBASE_API_KEY"),
		allowList: map[string]struct{}{},
	}
	for _, origin := range allowOrigins {
		api.allowList[origin] = struct{}{}
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// The owner's side: invite people, or take a share back.
	mux.Handle("POST /api/shares", api.authenticated(api.createShare))
	mux.Handle("DELETE /api/shares/{uid}", api.authenticated(api.revokeShare))

	// The recipient's side: accept an invitation, or drop one (whether it is
	// still pending or has already been accepted).
	mux.Handle("POST /api/shares/{uid}/accept", api.authenticated(api.acceptShare))
	mux.Handle("DELETE /api/shares/{uid}/inbox", api.authenticated(api.removeShare))

	return api.withCORS(mux)
}

// --- request/response plumbing ---------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("encoding response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// authenticated wraps a handler with Google sign-in verification.
//
// The browser sends the Firebase ID token it already holds; we exchange it at
// identitytoolkit to confirm it is genuine and learn which user it belongs to. We
// never trust a uid from the request body.
func (a *API) authenticated(next func(http.ResponseWriter, *http.Request, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.apiKey == "" {
			writeError(w, http.StatusServiceUnavailable, "FIREBASE_API_KEY is not configured")
			return
		}

		header := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}

		uid, err := verifyIDToken(r.Context(), a.apiKey, token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid session: "+err.Error())
			return
		}

		next(w, r, uid)
	})
}

// --- endpoints -------------------------------------------------------------

// createShare invites the named emails to read the caller's collection.
//
// Nothing is copied: the invitation is just a pointer to the caller's uid, and
// once accepted the rules let the recipient subscribe to the live collection.
// Recipients are emails; unknown ones are reported back so the sender can correct
// a typo rather than silently inviting nobody.
func (a *API) createShare(w http.ResponseWriter, r *http.Request, uid string) {
	var body struct {
		Recipients []string `json:"recipients"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.Recipients) == 0 {
		writeError(w, http.StatusBadRequest, "at least one recipient is required")
		return
	}
	if len(body.Recipients) > maxRecipients {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d recipients", maxRecipients))
		return
	}

	ctx := r.Context()

	// Resolve emails to uids. An unknown email is not an error on its own: it is
	// reported back and the invitation still goes to the ones that resolved.
	var targets []string
	var unknown []string
	seen := map[string]bool{}
	for _, raw := range body.Recipients {
		email := strings.ToLower(strings.TrimSpace(raw))
		if email == "" || seen[email] {
			continue
		}
		if !strings.Contains(email, "@") {
			unknown = append(unknown, email)
			continue
		}
		seen[email] = true

		target, err := a.db.UIDByEmail(ctx, email)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if target == "" || target == uid {
			unknown = append(unknown, email)
			continue
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		writeError(w, http.StatusBadRequest, "none of those emails belong to a CoinApp account")
		return
	}

	created := time.Now().UnixMilli()
	delivered := 0
	for _, target := range targets {
		// Already shared and accepted: nothing to do, but it is not a failure.
		var grant Relation
		if err := a.db.Get(ctx, "shares/"+target+"/"+uid, &grant); err != nil {
			log.Printf("checking existing grant for %s: %v", target, err)
		}
		if grant.AcceptedAt == 0 {
			if err := a.db.Put(ctx, "inbox/"+target+"/"+uid, Relation{CreatedAt: created}); err != nil {
				log.Printf("inviting %s: %v", target, err)
				continue
			}
			if err := a.db.Put(ctx, "outbox/"+uid+"/"+target, Relation{CreatedAt: created}); err != nil {
				log.Printf("recording outbox for %s: %v", target, err)
			}
		}
		delivered++
	}

	if delivered == 0 {
		writeError(w, http.StatusBadRequest, "none of those users could be reached")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"recipients": delivered,
		"unknown":    unknown,
	})
}

// acceptShare turns a pending invitation into a grant.
//
// It writes /shares/{reader}/{owner}, which is the node the security rules check
// before letting the reader see the owner's collection. It is idempotent: a
// second call returns the original timestamp rather than moving it, so a double
// tap or a retried request cannot double-count.
func (a *API) acceptShare(w http.ResponseWriter, r *http.Request, uid string) {
	owner := r.PathValue("uid")
	if owner == "" {
		writeError(w, http.StatusBadRequest, "missing owner id")
		return
	}

	ctx := r.Context()

	var invite Relation
	if err := a.db.Get(ctx, "inbox/"+uid+"/"+owner, &invite); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var grant Relation
	if err := a.db.Get(ctx, "shares/"+uid+"/"+owner, &grant); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if invite.CreatedAt == 0 && grant.AcceptedAt == 0 {
		writeError(w, http.StatusNotFound, "no such invitation")
		return
	}

	acceptedAt := grant.AcceptedAt
	if acceptedAt == 0 {
		acceptedAt = time.Now().UnixMilli()
		if err := a.db.Put(ctx, "shares/"+uid+"/"+owner, Relation{AcceptedAt: acceptedAt}); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	// The invitation has served its purpose; the grant now carries the state.
	if invite.CreatedAt != 0 {
		if err := a.db.Delete(ctx, "inbox/"+uid+"/"+owner); err != nil {
			log.Printf("clearing invite for %s: %v", owner, err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"acceptedAt": acceptedAt})
}

// removeShare drops a share the caller received, whether it is still pending (a
// rejection) or already accepted (a removal). Dropping the grant immediately ends
// the read access, because the rules re-evaluate on the next read.
func (a *API) removeShare(w http.ResponseWriter, r *http.Request, uid string) {
	owner := r.PathValue("uid")
	if owner == "" {
		writeError(w, http.StatusBadRequest, "missing owner id")
		return
	}

	ctx := r.Context()
	// Clear both possible states, and the owner's record of the relationship.
	for _, path := range []string{
		"inbox/" + uid + "/" + owner,
		"shares/" + uid + "/" + owner,
		"outbox/" + owner + "/" + uid,
	} {
		if err := a.db.Delete(ctx, path); err != nil {
			log.Printf("removing %s: %v", path, err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// revokeShare lets an owner take back a share they handed out, clearing the
// grant, the pending invitation (if it was never accepted) and their own record.
func (a *API) revokeShare(w http.ResponseWriter, r *http.Request, uid string) {
	reader := r.PathValue("uid")
	if reader == "" {
		writeError(w, http.StatusBadRequest, "missing reader id")
		return
	}

	ctx := r.Context()
	for _, path := range []string{
		"outbox/" + uid + "/" + reader,
		"inbox/" + reader + "/" + uid,
		"shares/" + reader + "/" + uid,
	} {
		if err := a.db.Delete(ctx, path); err != nil {
			log.Printf("revoking %s: %v", path, err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (a *API) withCORS(next http.Handler) http.Handler {
	if len(a.allowList) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if _, ok := a.allowList[origin]; ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// keep the import used even if the store is swapped out in tests.
var _ Store = (*firebase.Client)(nil)
