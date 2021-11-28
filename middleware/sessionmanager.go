package middleware

import (
	"context"
	"fmt"
	jwt2 "github.com/devtron-labs/authenticator/jwt"
	"github.com/devtron-labs/authenticator/oidc"
	jwt "github.com/golang-jwt/jwt/v4"
	log "github.com/sirupsen/logrus"
	"net"
	"net/http"
	"time"
)

// SessionManager generates and validates JWT tokens for login sessions.
type SessionManager struct {
	settings *oidc.Settings
	client   *http.Client
	prov     oidc.Provider
}

const (
	// SessionManagerClaimsIssuer fills the "iss" field of the token.
	SessionManagerClaimsIssuer = "argocd"

	// invalidLoginError, for security purposes, doesn't say whether the username or password was invalid.  This does not mitigate the potential for timing attacks to determine which is which.
	invalidLoginError  = "Invalid username or password"
	blankPasswordError = "Blank passwords are not allowed"
	badUserError       = "Bad local superuser username"
)

// NewSessionManager creates a new session manager from Argo CD settings
func NewSessionManager(settings *oidc.Settings, dexServerAddr string) *SessionManager {
	s := SessionManager{
		settings: settings,
	}
	s.client = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: nil,
			Proxy:           http.ProxyFromEnvironment,
			Dial: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).Dial,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
	s.client.Transport = oidc.NewDexRewriteURLRoundTripper(dexServerAddr, s.client.Transport)
	return &s
}

// Create creates a new token for a given subject (user) and returns it as a string.
// Passing a value of `0` for secondsBeforeExpiry creates a token that never expires.
func (mgr *SessionManager) Create(subject string, secondsBeforeExpiry int64) (string, error) {
	// Create a new token object, specifying signing method and the claims
	// you would like it to contain.
	now := time.Now().UTC()
	claims := jwt.StandardClaims{
		IssuedAt:  now.Unix(),
		Issuer:    SessionManagerClaimsIssuer,
		NotBefore: now.Unix(),
		Subject:   subject,
	}
	if secondsBeforeExpiry > 0 {
		expires := now.Add(time.Duration(secondsBeforeExpiry) * time.Second)
		claims.ExpiresAt = expires.Unix()
	}

	return mgr.signClaims(claims)
}

func (mgr *SessionManager) signClaims(claims jwt.Claims) (string, error) {
	log.Infof("Issuing claims: %v", claims)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(mgr.settings.OIDCConfig.ClientSecret)
}

// Parse tries to parse the provided string and returns the token claims for local superuser login.
func (mgr *SessionManager) Parse(tokenString string) (jwt.Claims, error) {
	// Parse takes the token string and a function for looking up the key. The latter is especially
	// useful if you use multiple keys for your application.  The standard is to use 'kid' in the
	// head of the token to identify which key to use, but the parsed token (head and claims) is provided
	// to the callback, providing flexibility.
	var claims jwt.MapClaims
	settings := mgr.settings

	token, err := jwt.ParseWithClaims(tokenString, &claims, func(token *jwt.Token) (interface{}, error) {
		// Don't forget to validate the alg is what you expect:
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}
		return settings.OIDCConfig.ClientSecret, nil
	})
	if err != nil {
		return nil, err
	}
	/*
		issuedAt := time.Unix(int64(claims["iat"].(float64)), 0)
		if issuedAt.Before(settings.AdminPasswordMtime) {
			return nil, fmt.Errorf("Password for superuser has changed since token issued")
		}*/
	return token.Claims, nil
}

// VerifyToken verifies if a token is correct. Tokens can be issued either from us or by an IDP.
// We choose how to verify based on the issuer.
func (mgr *SessionManager) VerifyToken(tokenString string) (jwt.Claims, error) {
	parser := &jwt.Parser{
		SkipClaimsValidation: true,
	}
	var claims jwt.RegisteredClaims
	_, _, err := parser.ParseUnverified(tokenString, &claims)
	if err != nil {
		return nil, err
	}
	switch claims.Issuer {
	case SessionManagerClaimsIssuer:
		// Argo CD signed token
		return mgr.Parse(tokenString)
	default:
		// IDP signed token
		prov, err := mgr.provider()
		if err != nil {
			return nil, err
		}
		idToken, err := prov.Verify(claims.Audience[0], tokenString)
		if err != nil {
			return nil, err
		}
		var claims jwt.MapClaims
		err = idToken.Claims(&claims)
		return claims, err
	}
}

func (mgr *SessionManager) provider() (oidc.Provider, error) {
	if mgr.prov != nil {
		return mgr.prov, nil
	}
	mgr.prov = oidc.NewOIDCProvider(mgr.settings.OIDCConfig.Issuer, mgr.client)
	return mgr.prov, nil
}

// Username is a helper to extract a human readable username from a context
func Username(ctx context.Context) string {
	claims, ok := ctx.Value("claims").(jwt.Claims)
	if !ok {
		return ""
	}
	mapClaims, err := jwt2.MapClaims(claims)
	if err != nil {
		return ""
	}
	switch jwt2.GetField(mapClaims, "iss") {
	case SessionManagerClaimsIssuer:
		return jwt2.GetField(mapClaims, "sub")
	default:
		return jwt2.GetField(mapClaims, "email")
	}
}