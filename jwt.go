// caddyjwt is a Caddy Module - who facilitates JWT authentication.
package caddyjwt

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/caddyauth"
	"github.com/golang-jwt/jwt"
	"github.com/lestrrat-go/jwx/v2/jwa"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(JWTAuth{})
}

type User = caddyauth.User
type Token = jwt.Token

// JWTAuth facilitates JWT (JSON Web Token) authentication.
type JWTAuth struct {
	// SignKey is the key used by the signing algorithm to verify the signature.
	//
	// For symmetric algorithems, use the key directly. e.g.
	//
	//     "<secret_key_bytes_in_base64_format>".
	//
	// For asymmetric algorithems, use the public key in x509 PEM format. e.g.
	//
	//     -----BEGIN PUBLIC KEY-----
	//     ...
	//     -----END PUBLIC KEY-----
	// This is an optional field. You can instead provide JWKURL to use JWKs.
	SignKey string `json:"sign_key"`

	// JWKURL is the URL where a provider publishes their JWKs. The URL must
	// publish the JWKs in the standard format as described in
	// https://tools.ietf.org/html/rfc7517.
	// If you'd like to use JWK, set this field and leave SignKey unset.
	JWKURL string `json:"jwk_url"`

	// SignAlgorithm is the the signing algorithm used. Available values are defined in
	// https://www.rfc-editor.org/rfc/rfc7518#section-3.1
	// This is an optional field, which is used for determining the signing algorithm.
	// We will try to determine the algorithm automatically from the following sources:
	// 1. The "alg" field in the JWT header.
	// 2. The "alg" field in the matched JWK (if JWKURL is provided).
	// 3. The value set here.
	SignAlgorithm string `json:"sign_alg"`

	// FromQuery defines a list of names to get tokens from the query parameters
	// of an HTTP request.
	//
	// If multiple keys were given, all the corresponding query
	// values will be treated as candidate tokens. And we will verify each of
	// them until we got a valid one.
	//
	// Priority: from_query > from_header > from_cookies.
	FromQuery []string `json:"from_query"`

	// FromHeader works like FromQuery. But defines a list of names to get
	// tokens from the HTTP header.
	FromHeader []string `json:"from_header"`

	// FromCookie works like FromQuery. But defines a list of names to get tokens
	// from the HTTP cookies.
	FromCookies []string `json:"from_cookies"`

	// IssuerWhitelist defines a list of issuers. A non-empty list turns on "iss
	// verification": the "iss" claim must exist in the given JWT payload. And
	// the value of the "iss" claim must be on the whitelist in order to pass
	// the verification.
	IssuerWhitelist []string `json:"issuer_whitelist"`

	// AudienceWhitelist defines a list of audiences. A non-empty list turns on
	// "aud verification": the "aud" claim must exist in the given JWT payload.
	// The verification will pass as long as one of the "aud" values is on the
	// whitelist.
	AudienceWhitelist []string `json:"audience_whitelist"`

	// UserClaims defines a list of names to find the ID of the authenticated user.
	//
	// By default, this config will be set to []string{"sub"}.
	//
	// If multiple names were given, we will use the first non-empty value of the key
	// in the JWT payload as the ID of the authenticated user. i.e. The placeholder
	// {http.auth.user.id} will be set to the ID.
	//
	// For example, []string{"uid", "username"} will set "eva" as the final user ID
	// from JWT payload: { "username": "eva"  }.
	//
	// If no non-empty values found, leaves it unauthenticated.
	UserClaims []string `json:"user_claims"`

	// MetaClaims defines a map to populate {http.auth.user.*} metadata placeholders.
	// The key is the claim in the JWT payload, the value is the placeholder name.
	// e.g. {"IsAdmin": "is_admin"} can populate {http.auth.user.is_admin} with
	// the value of `IsAdmin` in the JWT payload if found, otherwise "".
	//
	// NOTE: The name in the placeholder should be adhere to Caddy conventions
	// (snake_casing).
	//
	// Caddyfile:
	// Use syntax `<claim>[-> <placeholder>]` to define a map item. The placeholder is
	// optional, if not specified, use the same name as the claim.
	// e.g.
	//
	//     meta_claims "IsAdmin -> is_admin" "group"
	//
	// is equal to {"IsAdmin": "is_admin", "group": "group"}.
	//
	// Since v0.6.0, nested claim path is also supported, e.g.
	// For the following JWT payload:
	//
	//     { ..., "user_info": { "role": "admin" }}
	//
	// If you want to populate {http.auth.user.role} with "admin", you can use
	//
	//     meta_claims "user_info.role -> role"
	//
	// Use dot notation to access nested claims.
	MetaClaims map[string]string `json:"meta_claims"`

	logger        *zap.Logger
	parsedSignKey interface{} // can be []byte, *rsa.PublicKey, *ecdsa.PublicKey, etc.

}

// CaddyModule implements caddy.Module interface.
func (JWTAuth) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.authentication.providers.jwt",
		New: func() caddy.Module { return new(JWTAuth) },
	}
}

// Provision implements caddy.Provisioner interface.
func (ja *JWTAuth) Provision(ctx caddy.Context) error {
	ja.logger = ctx.Logger(ja)
	return nil
}

// Error implements httprc.ErrSink interface.
// It is used to log the error message provided by other modules, e.g. jwk.
func (ja *JWTAuth) Error(err error) {
	ja.logger.Error("error", zap.Error(err))
}

// Validate implements caddy.Validator interface.
func (ja *JWTAuth) Validate() error {
	if keyBytes, asymmetric, err := parseSignKey(ja.SignKey); err != nil {
		// Key(step 1): base64 -> raw bytes.
		return fmt.Errorf("invalid sign_key: %w", err)
	} else {
		// Key(step 2): raw bytes -> parsed key.
		if !asymmetric {
			ja.parsedSignKey = keyBytes
		} else if ja.parsedSignKey, err = x509.ParsePKIXPublicKey(keyBytes); err != nil {
			return fmt.Errorf("invalid sign_key (asymmetric): %w", err)
		}

		if ja.SignAlgorithm != "" {
			var alg jwa.SignatureAlgorithm
			if err := alg.Accept(ja.SignAlgorithm); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidSignAlgorithm, err)
			}
		}
	}

	if len(ja.UserClaims) == 0 {
		ja.UserClaims = []string{
			"sub",
			"user_id",
		}
	}
	fmt.Printf("user authenticated1")
	for claim, placeholder := range ja.MetaClaims {
		if claim == "" || placeholder == "" {
			return fmt.Errorf("invalid meta claim: %s -> %s", claim, placeholder)
		}
	}
	return nil
}

const (
	ClusterName string = "clustername"
	ClusterType string = "clustertype"
)

func getHeaderString(header map[string]interface{}, key string) (val string) {
	ret, ok := header[key]
	if !ok {
		return ""
	}
	res, ok := ret.(string)
	if !ok {
		return ""
	}
	return res
}

// Authenticate validates the JWT in the request and returns the user, if valid.
func (ja *JWTAuth) Authenticate(rw http.ResponseWriter, r *http.Request) (User, bool, error) {
	var (
		gotToken   *Token
		candidates []string
		err        error
	)

	candidates = append(candidates, getTokensFromQuery(r, ja.FromQuery)...)
	candidates = append(candidates, getTokensFromHeader(r, ja.FromHeader)...)
	candidates = append(candidates, getTokensFromCookies(r, ja.FromCookies)...)

	candidates = append(candidates, getTokensFromHeader(r, []string{"Authorization"})...)
	checked := make(map[string]struct{})

	for _, candidateToken := range candidates {
		tokenString := normToken(candidateToken)
		if _, ok := checked[tokenString]; ok {
			continue
		}

		gotToken, err = jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("there was an error")
			}
			return ja.parsedSignKey, nil
		})

		if err != nil {
			ja.logger.Error("parse token error", zap.Error(err), zap.String("bearerToken", tokenString))
			continue
		}
		clusterName := getHeaderString(gotToken.Header, ClusterName)
		clusterType := getHeaderString(gotToken.Header, ClusterType)

		checked[tokenString] = struct{}{}

		logger := ja.logger.With(zap.String("token_string", desensitizedTokenString(tokenString)))
		if err != nil {
			logger.Error("invalid token", zap.Error(err))
			continue
		}
		r.Header.Add("Cluster", clusterName)
		// By default, the following claims will be verified:
		//   - "exp"
		//   - "iat"
		//   - "nbf"
		// Here, if `aud_whitelist` or `iss_whitelist` were specified,
		// continue to verify "aud" and "iss" correspondingly.

		/*
			// The token is valid. Continue to check the user claim.
			claimName, gotUserID := getUserID(gotToken, ja.UserClaims)
			if gotUserID == "" {
				err = ErrEmptyUserClaim
				logger.Error("invalid token", zap.Strings("user_claims", ja.UserClaims), zap.Error(err))
				continue
			}

			// Successfully authenticated!
			var user = User{
				ID:       gotUserID,
				Metadata: getUserMetadata(gotToken, ja.MetaClaims),
			}
			logger.Info("user authenticated", zap.String("user_claim", claimName), zap.String("id", gotUserID))
		*/

		var user = User{
			ID: clusterName,
		}
		logger.Info("user authenticated", zap.String("cluster_type", clusterType), zap.String("cluster_name", user.ID))

		return user, true, nil
	}

	return User{}, false, err
}

func normToken(token string) string {
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = token[len("bearer "):]
	}
	return strings.TrimSpace(token)
}

func getTokensFromHeader(r *http.Request, names []string) []string {
	tokens := make([]string, 0)
	for _, key := range names {
		token := r.Header.Get(key)
		if token != "" {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

func getTokensFromQuery(r *http.Request, names []string) []string {
	tokens := make([]string, 0)
	for _, key := range names {
		token := r.FormValue(key)
		if token != "" {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

func getTokensFromCookies(r *http.Request, names []string) []string {
	tokens := make([]string, 0)
	for _, key := range names {
		if ck, err := r.Cookie(key); err == nil && ck.Value != "" {
			tokens = append(tokens, ck.Value)
		}
	}
	return tokens
}

func desensitizedTokenString(token string) string {
	if len(token) <= 6 {
		return token
	}
	mask := len(token) / 3
	if mask > 16 {
		mask = 16
	}
	return token[:mask] + "…" + token[len(token)-mask:]
}

// parseSignKey parses the given key and returns the key bytes.
func parseSignKey(signKey string) (keyBytes []byte, asymmetric bool, err error) {
	if len(signKey) == 0 {
		return nil, false, ErrMissingKeys
	}
	if strings.Contains(signKey, "-----BEGIN PUBLIC KEY-----") {
		keyBytes, err = parsePEMFormattedPublicKey(signKey)
		return keyBytes, true, err
	}
	keyBytes, err = base64.StdEncoding.DecodeString(signKey)
	return keyBytes, false, err
}

func parsePEMFormattedPublicKey(pubKey string) ([]byte, error) {
	block, _ := pem.Decode([]byte(pubKey))
	if block != nil && block.Type == "PUBLIC KEY" {
		return block.Bytes, nil
	}

	return nil, ErrInvalidPublicKey
}

// Interface guards
var (
	_ caddy.Provisioner       = (*JWTAuth)(nil)
	_ caddy.Validator         = (*JWTAuth)(nil)
	_ caddyauth.Authenticator = (*JWTAuth)(nil)
)
