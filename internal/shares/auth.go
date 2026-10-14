package shares

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// verifyIDToken confirms a Firebase ID token really was minted by this project,
// and returns the uid it belongs to.
//
// Identity Toolkit's accounts:lookup is used rather than decoding the token
// locally: the uid is then Google's answer rather than anything the client
// asserted, and there is no public-key set to fetch and cache. (The sibling
// securetoken endpoint only refreshes a refresh token; it cannot validate an ID
// token, so it is not used here.)
func verifyIDToken(ctx context.Context, apiKey, idToken string) (string, error) {
	if strings.TrimSpace(idToken) == "" {
		return "", fmt.Errorf("empty token")
	}

	endpoint := "https://identitytoolkit.googleapis.com/v1/accounts:lookup?key=" + url.QueryEscape(apiKey)
	payload, err := json.Marshal(map[string]string{"idToken": idToken})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := readAllLimited(resp)

	var decoded struct {
		Users []struct {
			LocalID string `json:"localId"`
		} `json:"users"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		if decoded.Error.Message != "" {
			return "", fmt.Errorf("token rejected: %s", decoded.Error.Message)
		}
		return "", fmt.Errorf("token rejected by accounts:lookup")
	}
	if len(decoded.Users) == 0 || decoded.Users[0].LocalID == "" {
		return "", fmt.Errorf("token carried no subject")
	}
	return decoded.Users[0].LocalID, nil
}

func readAllLimited(resp *http.Response) ([]byte, error) {
	const max = 1 << 20
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for len(buf) < max {
		n, err := resp.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			return buf, nil
		}
	}
	return buf, nil
}
