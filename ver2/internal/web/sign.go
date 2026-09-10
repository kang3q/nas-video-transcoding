package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Signed media URLs exist because AirPlay and a password cannot both work the
// ordinary way.
//
// Pressing AirPlay does not send the browser's picture anywhere. Safari hands
// the Apple TV a URL and the Apple TV fetches the file itself, as its own
// client, with none of the browser's credentials. Behind basic auth every one
// of those requests is a 401 and the television shows nothing, with no way for
// the viewer to log in — there is nowhere to type a password.
//
// So the bytes of video carry their own permission. A link minted by a page
// the viewer already authenticated for works for anything that follows it,
// including a television, and stops working a day later. Everything else —
// browsing the library, starting a conversion, cancelling one — stays behind
// the password, which is what actually needs protecting.
const signParam = "k"

// signTTL is how long a minted link lasts. Long enough for an episode watched
// from the middle of the evening, short enough that a link left in a chat or a
// screenshot is not a permanent hole.
const signTTL = 24 * time.Hour

// loadSecret reads the signing key, making one the first time. It lives in the
// state directory so links survive a restart; a key regenerated on every boot
// would invalidate the link in a notification the moment the container was
// updated.
func loadSecret(stateDir, configured string) ([]byte, error) {
	if configured != "" {
		return []byte(configured), nil
	}
	path := filepath.Join(stateDir, "signing-key")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	hexed := []byte(hex.EncodeToString(key))
	if err := os.WriteFile(path, hexed, 0o600); err != nil {
		return nil, fmt.Errorf("writing the signing key: %w", err)
	}
	return hexed, nil
}

// sign returns the path with a token attached, or unchanged when there is no
// password to work around. An unguarded server gains nothing from tokens and
// loses readable URLs.
func (s *Server) sign(path string) string {
	if s.cfg.User == "" {
		return path
	}
	exp := time.Now().Add(signTTL).Unix()
	u := &url.URL{Path: path}
	q := u.Query()
	q.Set(signParam, fmt.Sprintf("%d.%s", exp, mac(s.secret, path, exp)))
	u.RawQuery = q.Encode()
	return u.String()
}

// signedOK reports whether a request carries a token good for its own path.
// The path is part of what is signed, so a token for one file cannot be moved
// to another.
func (s *Server) signedOK(path, token string) bool {
	expText, sum, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expText, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(sum), []byte(mac(s.secret, path, exp)))
}

func mac(secret []byte, path string, exp int64) string {
	h := hmac.New(sha256.New, secret)
	fmt.Fprintf(h, "%s\n%d", path, exp)
	return hex.EncodeToString(h.Sum(nil))[:32]
}
