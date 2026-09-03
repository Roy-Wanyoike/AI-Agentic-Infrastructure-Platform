package sso

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file contains the hand-rolled, stdlib-only OIDC wire layer:
//
//   - discovery document fetching with a per-issuer TTL cache;
//   - JWKS fetching + caching + RSA key reconstruction (base64rawurl n/e);
//   - the authorization-code exchange (client_secret_post form);
//   - RS256 JOSE verification: header parsing (alg pinned to RS256 — no
//     "none"/HMAC confusion), PKCS#1 v1.5 signature over the SHA-256 digest
//     of the signing input, then iss/aud/exp claim validation.
//
// No third-party JOSE library is used anywhere (go.mod stays untouched).

const (
	defaultLeeway = 30 * time.Second // clock skew allowance for exp
	httpMaxBody   = 1 << 20          // 1 MiB cap on discovery/JWKS/token bodies
)

type cacheEntry[T any] struct {
	value   T
	fetched time.Time
}

// discoveryDocument is the subset of the OIDC discovery metadata the flow
// needs (https://openid.net/specs/openid-connect-discovery-1_0.html).
type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	UserInfoEndpoint      string `json:"userinfo_endpoint"`
}

// jwksDocument is the JSON Web Key Set (RFC 7517). Only RSA signing keys are
// consumed; everything else is ignored.
type jwksDocument struct {
	Keys []jsonWebKey `json:"keys"`
}

type jsonWebKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// tokenEndpointResponse is the subset of the OAuth2 token response used here.
type tokenEndpointResponse struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

// idTokenHeader is the decoded JOSE header. alg MUST be RS256; anything else
// is rejected before any key material is touched.
type idTokenHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// idTokenClaims is the decoded payload subset.
type idTokenClaims struct {
	Issuer   string          `json:"iss"`
	Subject  string          `json:"sub"`
	Audience json.RawMessage `json:"aud"` // string or []string per JWT spec
	Expiry   float64         `json:"exp"`
	IssuedAt float64         `json:"iat"`
	Nonce    string          `json:"nonce"`
	Email    string          `json:"email"`
}

// audiences decodes the aud claim (single string or array form).
func (c *idTokenClaims) audiences() []string {
	var out []string
	if len(c.Audience) == 0 {
		return out
	}
	var single string
	if err := json.Unmarshal(c.Audience, &single); err == nil {
		return append(out, single)
	}
	var multi []string
	if err := json.Unmarshal(c.Audience, &multi); err == nil {
		return append(out, multi...)
	}
	return out
}

// discover fetches (or serves from cache) the issuer's discovery document.
// The cached doc is stamped against the issuer on every use so a config
// pointing at a different issuer cannot ride an old cache entry.
func (s *Service) fetchDiscovery(ctx context.Context, issuer string) (*discoveryDocument, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if issuer == "" {
		return nil, ErrIssuerRequired
	}
	s.mu.Lock()
	if entry, ok := s.discover[issuer]; ok && s.now().Sub(entry.fetched) < discoveryCacheTTL {
		doc := entry.value
		s.mu.Unlock()
		return &doc, nil
	}
	s.mu.Unlock()

	endpoint := issuer + "/.well-known/openid-configuration"
	var doc discoveryDocument
	if err := s.getJSON(ctx, endpoint, &doc); err != nil {
		return nil, err
	}
	if strings.TrimSpace(doc.Issuer) != "" &&
		strings.TrimRight(doc.Issuer, "/") != issuer {
		return nil, fmt.Errorf("discovery issuer %q does not match configured issuer %q", doc.Issuer, issuer)
	}
	if doc.TokenEndpoint == "" || doc.JWKSURI == "" {
		return nil, errors.New("discovery document missing token_endpoint or jwks_uri")
	}
	s.mu.Lock()
	s.discover[issuer] = cacheEntry[discoveryDocument]{value: doc, fetched: s.now()}
	s.mu.Unlock()
	return &doc, nil
}

// publicKeyFor resolves the RSA verification key for a kid from the cached
// JWKS, refreshing the cache once when the kid is unknown (IdP key rotation).
func (s *Service) publicKeyFor(ctx context.Context, doc *discoveryDocument, kid string) (*rsa.PublicKey, error) {
	key := func(set *jwksDocument) *rsa.PublicKey {
		for _, k := range set.Keys {
			if k.Kty != "RSA" || k.Kid != kid {
				continue
			}
			if pub, err := k.rsaPublicKey(); err == nil {
				return pub
			}
		}
		return nil
	}

	s.mu.Lock()
	entry, cached := s.jwks[doc.JWKSURI]
	fresh := cached && s.now().Sub(entry.fetched) < discoveryCacheTTL
	var set jwksDocument
	if fresh {
		set = entry.value
	}
	s.mu.Unlock()

	if fresh {
		if pub := key(&set); pub != nil {
			return pub, nil
		}
	}

	// Cache miss, stale entry, or unknown kid (rotation): force refresh.
	var refreshed jwksDocument
	if err := s.getJSON(ctx, doc.JWKSURI, &refreshed); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.jwks[doc.JWKSURI] = cacheEntry[jwksDocument]{value: refreshed, fetched: s.now()}
	s.mu.Unlock()
	if pub := key(&refreshed); pub != nil {
		return pub, nil
	}
	return nil, fmt.Errorf("no RSA key for kid %q in JWKS", kid)
}

// rsaPublicKey reconstructs an rsa.PublicKey from the base64rawurl n/e
// parameters (RFC 7518 section 6.3).
func (k jsonWebKey) rsaPublicKey() (*rsa.PublicKey, error) {
	if k.Kty != "RSA" {
		return nil, errors.New("jwk is not RSA")
	}
	nb, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil || len(nb) == 0 {
		return nil, errors.New("jwk has invalid n")
	}
	eb, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil || len(eb) == 0 {
		return nil, errors.New("jwk has invalid e")
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nb),
		E: int(new(big.Int).SetBytes(eb).Int64()),
	}, nil
}

// exchangeCode performs the authorization-code exchange: an HTTP form POST to
// the token endpoint (client_secret_post) per the OAuth2 profile used by the
// platform. The ID token is extracted from the response.
func (s *Service) exchangeCode(ctx context.Context, tokenEndpoint, code, redirectURI, clientID, clientSecret string) (string, error) {
	if strings.TrimSpace(tokenEndpoint) == "" {
		return "", errors.New("token endpoint is not configured")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token endpoint request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: token endpoint returned %d", ErrIDTokenInvalid, resp.StatusCode)
	}
	var tok tokenEndpointResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("token response is not JSON: %w", err)
	}
	if strings.TrimSpace(tok.IDToken) == "" {
		return "", fmt.Errorf("%w: token response has no id_token", ErrIDTokenInvalid)
	}
	return tok.IDToken, nil
}

// verifyIDToken validates the compact JWS end-to-end and returns the claims:
// structural checks (3 segments), header alg pinned to RS256, signature
// verification via the issuer's JWKS, then iss/aud/exp validation. Nonce is
// checked by the caller against the single-use state entry.
func (s *Service) verifyIDToken(ctx context.Context, rawToken string, doc *discoveryDocument, cfg *SSOConfig) (*idTokenClaims, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, fmt.Errorf("%w: malformed JWT", ErrIDTokenInvalid)
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: undecodable JWT header", ErrIDTokenInvalid)
	}
	var header idTokenHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("%w: undecodable JWT header", ErrIDTokenInvalid)
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("%w: unsupported alg %q (only RS256)", ErrIDTokenInvalid, header.Alg)
	}

	pub, err := s.publicKeyFor(ctx, doc, header.Kid)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIDTokenInvalid, err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: undecodable JWT signature", ErrIDTokenInvalid)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], signature); err != nil {
		return nil, fmt.Errorf("%w: signature verification failed", ErrIDTokenInvalid)
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: undecodable JWT payload", ErrIDTokenInvalid)
	}
	var claims idTokenClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, fmt.Errorf("%w: undecodable JWT claims", ErrIDTokenInvalid)
	}

	if strings.TrimRight(claims.Issuer, "/") != strings.TrimRight(cfg.Issuer, "/") {
		return nil, fmt.Errorf("%w: issuer mismatch", ErrIDTokenInvalid)
	}
	audOK := false
	for _, aud := range claims.audiences() {
		if aud == cfg.ClientID {
			audOK = true
			break
		}
	}
	if !audOK {
		return nil, fmt.Errorf("%w: audience mismatch", ErrIDTokenInvalid)
	}
	now := s.now()
	if claims.Expiry == 0 || now.After(time.Unix(int64(claims.Expiry), 0).Add(defaultLeeway)) {
		return nil, fmt.Errorf("%w: token expired", ErrIDTokenInvalid)
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return nil, fmt.Errorf("%w: missing sub claim", ErrIDTokenInvalid)
	}
	return &claims, nil
}

// getJSON GETs a URL and decodes a bounded JSON body.
func (s *Service) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", endpoint, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s failed: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if err != nil {
		return fmt.Errorf("read %s failed: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s returned status %d", endpoint, resp.StatusCode)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode %s failed: %w", endpoint, err)
	}
	return nil
}
