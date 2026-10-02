package chatgptauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// clockSkew is the tolerance on an id_token's times (plan 033 §3.10: exp
// ±5 s), for a clock a little off the server's.
const clockSkew = 5 * time.Second

// minRSABits is the smallest JWKS key an id_token is checked against.
const minRSABits = 2048

// errIDToken is any id_token that does not validate. Which check failed is
// the error's text, never a claim's value.
var errIDToken = errors.New("chatgptauth: the sign-in's id_token is not valid")

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwks is the issuer's signing keys, by key id.
type jwks struct {
	keys map[string]*rsa.PublicKey // kid → key; "" for a key with no kid
}

// fetchJWKS fetches the signing keys from the discovery document's jwks_uri,
// which discover has already confined to the issuer's origin. Only RSA
// signing keys of at least minRSABits are kept.
func (e endpoints) fetchJWKS(ctx context.Context) (*jwks, error) {
	doc, err := e.discover(ctx)
	if err != nil {
		return nil, err
	}
	if doc.JWKSURI == "" {
		return nil, errors.New("chatgptauth: discovery: the OpenID configuration names no jwks_uri")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, doc.JWKSURI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	status, _, body, err := do(ctx, "jwks", req, maxReply)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &OAuthError{Step: "jwks", Status: status}
	}
	return parseJWKS(body)
}

func parseJWKS(body []byte) (*jwks, error) {
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, errors.New("chatgptauth: jwks: the key set is not JSON")
	}
	out := &jwks{keys: map[string]*rsa.PublicKey{}}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		nb, err1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(eb) == 0 || len(eb) > 4 {
			continue
		}
		n := new(big.Int).SetBytes(nb)
		e := int(new(big.Int).SetBytes(eb).Int64())
		if n.BitLen() < minRSABits || e < 3 || e%2 == 0 {
			continue
		}
		out.keys[k.Kid] = &rsa.PublicKey{N: n, E: e}
	}
	if len(out.keys) == 0 {
		return nil, errors.New("chatgptauth: jwks: the key set has no RSA signing key")
	}
	return out, nil
}

// key is the key a token's kid names. A token with no kid is checked against
// the only key, when there is exactly one.
func (j *jwks) key(kid string) *rsa.PublicKey {
	if k, ok := j.keys[kid]; ok {
		return k
	}
	if kid == "" && len(j.keys) == 1 {
		for _, k := range j.keys {
			return k
		}
	}
	return nil
}

// idClaims is what craze reads of a validated id_token.
type idClaims struct {
	Subject string
	Email   string
}

// idWant is what an id_token must carry: our issuer, the issued client id as
// its audience, this attempt's nonce, and (on a re-login) the saved
// account's subject.
type idWant struct {
	issuer   string
	clientID string
	nonce    string
	subject  string // "" on a first registration
}

// verifyIDToken validates raw (plan 033 §3.10, the sign-in docs' step 4): an
// RS256 signature by one of keys, then iss, aud (= the issued client id, with
// azp when there are several audiences), exp and nbf within clockSkew of
// now, the nonce, and a subject — the saved one on a re-login. The error says
// which check failed; no claim's value is ever in it.
func verifyIDToken(raw string, keys *jwks, want idWant, now time.Time) (idClaims, error) {
	fail := func(what string) (idClaims, error) {
		return idClaims{}, errors.New(errIDToken.Error() + ": " + what)
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return fail("not a signed JWT")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &hdr) != nil {
		return fail("its header does not decode")
	}
	if hdr.Alg != "RS256" {
		return fail("it is not signed with RS256")
	}
	key := keys.key(hdr.Kid)
	if key == nil {
		return fail("no key in the issuer's key set signed it")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fail("its signature does not decode")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return fail("its signature does not verify")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fail("its claims do not decode")
	}
	var c struct {
		Iss   string          `json:"iss"`
		Aud   json.RawMessage `json:"aud"`
		Azp   string          `json:"azp"`
		Exp   *json.Number    `json:"exp"`
		Nbf   *json.Number    `json:"nbf"`
		Nonce string          `json:"nonce"`
		Sub   string          `json:"sub"`
		Email string          `json:"email"`
	}
	if err := json.Unmarshal(pb, &c); err != nil {
		return fail("its claims do not decode")
	}
	if c.Iss != want.issuer {
		return fail("another issuer issued it")
	}
	var auds []string
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		auds = []string{one}
	} else if json.Unmarshal(c.Aud, &auds) != nil {
		return fail("its audience does not decode")
	}
	found := false
	for _, a := range auds {
		found = found || a == want.clientID
	}
	if !found || (len(auds) > 1 && c.Azp != want.clientID) {
		return fail("it is not for craze's client id")
	}
	exp, ok := unixTime(c.Exp)
	if !ok {
		return fail("it has no expiry")
	}
	if !now.Before(exp.Add(clockSkew)) {
		return fail("it has expired")
	}
	if nbf, ok := unixTime(c.Nbf); ok && now.Add(clockSkew).Before(nbf) {
		return fail("it is not valid yet")
	}
	if want.nonce == "" || c.Nonce != want.nonce {
		return fail("its nonce is not this sign-in's")
	}
	if c.Sub == "" {
		return fail("it names no account")
	}
	if want.subject != "" && c.Sub != want.subject {
		return fail("it is for another account than the one craze is registered with")
	}
	return idClaims{Subject: c.Sub, Email: c.Email}, nil
}

// unixTime is a JWT NumericDate.
func unixTime(n *json.Number) (time.Time, bool) {
	if n == nil {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f <= 0 || f > 1e11 {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// accessClientID is the client_id claim of an access token that is a JWT
// carrying one (the token reference's claims), and "" otherwise: the docs
// call the token's contents opaque, so a token that does not decode is not
// refused, but one that names another client is (refreshVerify).
func accessClientID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		ClientID string `json:"client_id"`
	}
	if json.Unmarshal(pb, &c) != nil {
		return ""
	}
	return c.ClientID
}
