package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// refreshMargin is how long before the login expires it gets refreshed.
const refreshMargin = 30 * time.Minute

// session is the signed-in user. Downloads authenticate with subscription
// details fetched at sign-in, but user playlists use the legacy token inside
// the login token, so a long run refreshes the login before it expires.
type session struct {
	token         string
	refreshToken  string // empty for a token from .env, which can't be refreshed
	expiry        time.Time
	legacyToken   string
	uguID         string
	warnedExpired bool
}

func newSession(token, refreshToken string) (*session, error) {
	s := &session{refreshToken: refreshToken}
	if err := s.setToken(token); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *session) setToken(token string) error {
	p, err := parseTokenPayload(token)
	if err != nil {
		return err
	}
	s.token, s.legacyToken, s.uguID = token, p.LegacyToken, p.LegacyUguid
	s.expiry = time.Time{}
	if p.Exp > 0 {
		s.expiry = time.Unix(int64(p.Exp), 0)
	}
	return nil
}

// refreshIfNeeded renews the login if it expires within refreshMargin. A
// failure is reported but not fatal, since most downloads don't need it.
func (s *session) refreshIfNeeded(ctx context.Context) {
	if s.expiry.IsZero() || time.Until(s.expiry) > refreshMargin {
		return
	}
	if s.refreshToken == "" {
		if !s.warnedExpired && time.Now().After(s.expiry) {
			fmt.Fprintln(os.Stderr, "Warning: your token has expired, so user playlists may fail. See token.md to get a new one.")
			s.warnedExpired = true
		}
		return
	}
	a, err := refreshAuth(ctx, s.refreshToken)
	if err == nil {
		err = s.setToken(a.AccessToken)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Warning: couldn't refresh the login:", err)
		return
	}
	if a.RefreshToken != "" {
		s.refreshToken = a.RefreshToken
	}
	fmt.Println("Refreshed the login.")
}

func tokenRequest(ctx context.Context, form url.Values) (*Auth, error) {
	form.Set("client_id", clientId)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authUrl, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var obj Auth
	if err := doJSON(req, userAgent, &obj); err != nil {
		return nil, err
	}
	return &obj, nil
}

func auth(ctx context.Context, email, pwd string) (*Auth, error) {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("scope", "openid profile email nugsnet:api nugsnet:legacyapi offline_access")
	form.Set("username", email)
	form.Set("password", pwd)
	return tokenRequest(ctx, form)
}

func refreshAuth(ctx context.Context, refreshToken string) (*Auth, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	return tokenRequest(ctx, form)
}
