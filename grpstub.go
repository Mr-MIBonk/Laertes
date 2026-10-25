// Development stand-in for the VW GRP sign-in service
// (grp-prelive.volkswagenag.com) that ODIS drives during an online login, so the whole login
// handshake can be exercised against 127.0.0.1 without contacting VW. It talks to nothing real and
// changes nothing; it only answers.
//
// It implements every endpoint advertised in its own OpenID discovery document:
//
//	/.well-known/openid-configuration  discovery
//	/isam/sps/oauth/oauth20/authorize  authorization endpoint (authorization_code, with PKCE)
//	/isam/sps/oauth/oauth20/token      token endpoint (authorization_code, client_credentials, refresh_token)
//	/isam/sps/oauth/oauth20/userinfo   claims for a bearer access token
//	/isam/sps/oauth/oauth20/jwks/...   public key the id_token (RS256) is signed with
//	/isam/sps/oauth/oauth20/introspect token introspection
//	/isam/sps/oauth/oauth20/revoke     token revocation
//
// The authorization endpoint shows a sign-in form (or takes username and password posted to it)
// and redirects back with a code. Everything else follows OAuth 2.0 / OIDC closely enough that a
// conforming client signs in: the id_token is a real
// RS256-signed JWT whose iss matches the discovery issuer and whose signature verifies against the
// served JWKS. Stdlib only.
//
// Laertes serves it with -grpstub, on 127.0.0.1:23032 (ODIS's "grp.discovery" must point there).
// Account admin/admin.

package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// stubBase is where these endpoints live; issuer is what the id_token claims as iss (and what the
// discovery document advertises). A conforming client checks id_token.iss == discovery issuer, so
// the two are single-sourced here and nowhere else.
const (
	stubAddr   = "127.0.0.1:23032"
	stubBase   = "http://" + stubAddr
	stubIssuer = "https://grp-prelive.volkswagenag.com"

	stubTokenTTL = time.Hour
	stubCodeTTL  = 5 * time.Minute
)

// stubKid names the signing key. The key is new on every start, so its name is too: ODIS caches the
// key by name, and under a reused name it keeps the previous run's key and skips its token checks.
var stubKid = "stub-" + time.Now().Format("20060102150405")

// The accounts that may sign in, username -> password. A dev stub: one account, admin/admin.
var stubUsers = map[string]string{"admin": "admin"}

// claims describes the signed-in account in the token and userinfo. sub is filled with the
// authenticated username per login.
var stubClaims = map[string]any{
	"sub":               "admin",
	"email":             "admin@example.invalid",
	"emailAddress":      "admin@example.invalid",
	"mail":              "admin@example.invalid",
	"given_name":        "Admin",
	"givenname":         "Admin",
	"family_name":       "User",
	"sn":                "User",
	"name":              "Admin User",
	"salespersonnumber": "0000",
	// How strongly the user signed in. ODIS logs it as "recv alevel=..." and denies level 1
	// ("weak authentication detected"); 2 passes.
	"authlevel": "2",
}

type stubCode struct {
	clientID, redirectURI, scope, nonce, challenge, method, sub string
	expires                                                     time.Time
}

type stubToken struct {
	sub, scope, clientID string
	expires              time.Time
	revoked              bool
}

type grpStub struct {
	key *rsa.PrivateKey

	mu      sync.Mutex
	codes   map[string]stubCode
	access  map[string]stubToken // bearer access tokens, by value
	refresh map[string]stubToken // refresh tokens, by value
}

// serveGRPStub serves the stub on stubAddr in the background; a failure is logged, not fatal,
// since Laertes itself does not need it.
func serveGRPStub() {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Printf("GRP stub: generate signing key: %v", err)
		return
	}
	s := &grpStub{
		key:     key,
		codes:   map[string]stubCode{},
		access:  map[string]stubToken{},
		refresh: map[string]stubToken{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", stubLogged(s.discovery))
	mux.HandleFunc("/isam/sps/oauth/oauth20/authorize", stubLogged(s.authorize))
	mux.HandleFunc("/isam/sps/oauth/oauth20/token", stubLogged(s.token))
	mux.HandleFunc("/isam/sps/oauth/oauth20/userinfo", stubLogged(s.userinfo))
	mux.HandleFunc("/isam/sps/oauth/oauth20/jwks/oidcglobal", stubLogged(s.jwks))
	mux.HandleFunc("/isam/sps/oauth/oauth20/introspect", stubLogged(s.introspect))
	mux.HandleFunc("/isam/sps/oauth/oauth20/revoke", stubLogged(s.revoke))
	mux.HandleFunc("/cb", stubLogged(s.callback)) // built-in redirect target so a browser login shows a token
	// VW's GSB (web-service gateway): guided functions call it for online operations such as
	// component protection (PinService ProcessPin). Laertes's bridge points ODIS's url.*.gsb here
	// when this stub runs; the stub answers success so a function can complete without VW's backend.
	mux.HandleFunc("/services", stubLogged(s.gsb))
	// Everything else: an empty 200, as before, so unmapped ODIS probes do not fail the flow.
	mux.HandleFunc("/", stubLogged(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	ln, err := net.Listen("tcp", stubAddr)
	if err != nil {
		log.Printf("GRP stub: %v", err)
		return
	}
	log.Printf("GRP stub (VW sign-in stand-in) on %s, issuer %s, account admin/admin", stubBase, stubIssuer)
	go http.Serve(ln, mux)
}

// stubLogged prints every request (never the form's secret fields) before handling it.
func stubLogged(next http.HandlerFunc) http.HandlerFunc {
	secret := map[string]bool{"password": true, "client_secret": true, "code_verifier": true}
	return func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		shown := url.Values{}
		for k, v := range r.Form {
			if secret[k] {
				shown[k] = []string{"(set)"}
			} else {
				shown[k] = v
			}
		}
		log.Printf("%s %s  params=%v auth=%q", r.Method, r.URL.Path, shown, r.Header.Get("Authorization"))
		next(w, r)
	}
}

func (s *grpStub) discovery(w http.ResponseWriter, r *http.Request) {
	e := stubBase + "/isam/sps/oauth/oauth20"
	stubJSON(w, http.StatusOK, map[string]any{
		"issuer":                 stubIssuer,
		"authorization_endpoint": e + "/authorize",
		"token_endpoint":         e + "/token",
		"userinfo_endpoint":      e + "/userinfo",
		"jwks_uri":               e + "/jwks/oidcglobal",
		"introspection_endpoint": e + "/introspect",
		"revocation_endpoint":    e + "/revoke",
		"response_types_supported": []string{
			"token", "id_token", "token id_token", "code",
			"code id_token", "code token id_token", "code token", "none"},
		"response_modes_supported":              []string{"query", "fragment", "form_post"},
		"grant_types_supported":                 []string{"client_credentials", "authorization_code", "refresh_token"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"subject_types_supported":               []string{"public"},
		"scopes_supported":                      []string{"openid", "profile", "email", "mail", "sn", "given_name", "family_name", "salespersonnumber"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "none"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
		"claims_supported":                      []string{"sub", "email", "emailAddress", "mail", "given_name", "givenname", "family_name", "sn", "name", "salespersonnumber"},
		"request_parameter_supported":           true,
		"claims_parameter_supported":            true,
	})
}

// authorize signs the user in: with no credentials yet it shows a sign-in form; once the username
// and password match a known account it mints an authorization code bound to the request's PKCE
// challenge, nonce and scope, then redirects it back to the client.
func (s *grpStub) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.Form
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" {
		stubErr(w, http.StatusBadRequest, "invalid_request", "redirect_uri is required")
		return
	}
	state := q.Get("state")

	user, pass := q.Get("username"), q.Get("password")
	if user == "" && pass == "" {
		stubLoginForm(w, q, "") // first visit: ask for credentials
		return
	}
	if want, ok := stubUsers[user]; !ok || pass != want {
		stubLoginForm(w, q, "Wrong username or password.")
		return
	}
	sub := user

	code := stubRandom()
	s.mu.Lock()
	s.codes[code] = stubCode{
		clientID:    q.Get("client_id"),
		redirectURI: redirectURI,
		scope:       q.Get("scope"),
		nonce:       q.Get("nonce"),
		challenge:   q.Get("code_challenge"),
		method:      q.Get("code_challenge_method"),
		sub:         sub,
		expires:     time.Now().Add(stubCodeTTL),
	}
	s.mu.Unlock()

	u, err := url.Parse(redirectURI)
	if err != nil {
		stubErr(w, http.StatusBadRequest, "invalid_request", "redirect_uri is not a URL")
		return
	}
	rq := u.Query()
	rq.Set("code", code)
	if state != "" {
		rq.Set("state", state)
	}
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// stubLoginForm renders a minimal sign-in page that POSTs username/password back to the authorization
// endpoint while carrying the OAuth parameters through as hidden fields.
func stubLoginForm(w http.ResponseWriter, q url.Values, errMsg string) {
	hidden := ""
	for _, k := range []string{"client_id", "redirect_uri", "response_type", "scope", "state", "nonce", "code_challenge", "code_challenge_method"} {
		if v := q.Get(k); v != "" {
			hidden += `<input type="hidden" name="` + html.EscapeString(k) + `" value="` + html.EscapeString(v) + `">`
		}
	}
	errHTML := ""
	if errMsg != "" {
		errHTML = `<p style="color:#b00">` + html.EscapeString(errMsg) + `</p>`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>Sign in (stub)</title>`+
		`<body style="font-family:sans-serif;max-width:20rem;margin:3rem auto">`+
		`<h2>Sign in</h2>`+errHTML+
		`<form method="post" action="/isam/sps/oauth/oauth20/authorize">`+hidden+
		`<p><label>Username<br><input name="username" autofocus></label>`+
		`<p><label>Password<br><input name="password" type="password"></label>`+
		`<p><button type="submit">Sign in</button></form></body>`)
}

func (s *grpStub) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		stubErr(w, http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	clientID := r.Form.Get("client_id")
	if clientID == "" {
		if u, _, ok := r.BasicAuth(); ok {
			clientID = u
		}
	}
	switch gt := r.Form.Get("grant_type"); gt {
	case "authorization_code":
		s.tokenFromCode(w, r, clientID)
	case "refresh_token":
		s.tokenFromRefresh(w, r, clientID)
	case "client_credentials":
		stubJSON(w, http.StatusOK, s.grant(stubClaims["sub"].(string), r.Form.Get("scope"), clientID, "", false))
	default:
		stubErr(w, http.StatusBadRequest, "unsupported_grant_type", "unknown grant_type "+gt)
	}
}

func (s *grpStub) tokenFromCode(w http.ResponseWriter, r *http.Request, clientID string) {
	c, ok := s.consumeCode(r.Form.Get("code"))
	if !ok {
		stubErr(w, http.StatusBadRequest, "invalid_grant", "unknown or expired authorization code")
		return
	}
	if ru := r.Form.Get("redirect_uri"); ru != "" && ru != c.redirectURI {
		stubErr(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	if c.challenge != "" && !stubVerifyPKCE(c.challenge, c.method, r.Form.Get("code_verifier")) {
		stubErr(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	if clientID == "" {
		clientID = c.clientID
	}
	stubJSON(w, http.StatusOK, s.grant(c.sub, c.scope, clientID, c.nonce, stubWantsID(c.scope)))
}

func (s *grpStub) tokenFromRefresh(w http.ResponseWriter, r *http.Request, clientID string) {
	rt := r.Form.Get("refresh_token")
	s.mu.Lock()
	t, ok := s.refresh[rt]
	s.mu.Unlock()
	if !ok || t.revoked {
		stubErr(w, http.StatusBadRequest, "invalid_grant", "unknown or revoked refresh token")
		return
	}
	if clientID == "" {
		clientID = t.clientID
	}
	stubJSON(w, http.StatusOK, s.grant(t.sub, t.scope, clientID, "", stubWantsID(t.scope)))
}

// consumeCode looks up and removes an authorization code (single use), reporting whether it was
// present and unexpired.
func (s *grpStub) consumeCode(code string) (stubCode, bool) {
	s.mu.Lock()
	c, ok := s.codes[code]
	delete(s.codes, code)
	s.mu.Unlock()
	if !ok || time.Now().After(c.expires) {
		return stubCode{}, false
	}
	return c, true
}

// stubWantsID reports whether a token response for this scope should carry an id_token.
func stubWantsID(scope string) bool { return strings.Contains(scope, "openid") || scope == "" }

// grant stores a fresh access+refresh token for sub and returns the token-endpoint response,
// including an RS256 id_token when withID.
func (s *grpStub) grant(sub, scope, clientID, nonce string, withID bool) map[string]any {
	at, rt := stubRandom(), stubRandom()
	t := stubToken{sub: sub, scope: scope, clientID: clientID, expires: time.Now().Add(stubTokenTTL)}
	s.mu.Lock()
	s.access[at] = t
	s.refresh[rt] = t
	s.mu.Unlock()

	resp := map[string]any{
		"access_token":  at,
		"token_type":    "bearer", // as ISAM sends it; ODIS compares it case-sensitively
		"expires_in":    int(stubTokenTTL.Seconds()),
		"refresh_token": rt,
		"scope":         scope,
	}
	if withID {
		resp["id_token"] = s.idToken(sub, clientID, nonce)
	}
	return resp
}

// callback is a built-in redirect target for browser logins: it receives the authorization code,
// exchanges it for tokens server-side, and shows them, so a sign-in round-trips to a visible token
// without a separate client. Point redirect_uri at it: http://127.0.0.1:23032/cb.
func (s *grpStub) callback(w http.ResponseWriter, r *http.Request) {
	if e := r.Form.Get("error"); e != "" {
		stubErr(w, http.StatusBadRequest, e, r.Form.Get("error_description"))
		return
	}
	c, ok := s.consumeCode(r.Form.Get("code"))
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "<!doctype html><meta charset=utf-8><body style='font-family:sans-serif'>"+
			"<h2>No token</h2><p>Missing or expired authorization code. Start again at the sign-in link.</body>")
		return
	}
	resp := s.grant(c.sub, c.scope, c.clientID, c.nonce, stubWantsID(c.scope))
	pretty, _ := json.MarshalIndent(resp, "", "  ")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, "<!doctype html><meta charset=utf-8><title>Signed in (stub)</title>"+
		"<body style='font-family:sans-serif;max-width:48rem;margin:3rem auto'>"+
		"<h2>Signed in as "+html.EscapeString(c.sub)+"</h2>"+
		"<p>Token endpoint response (the client receives this by exchanging the code):"+
		"<pre style='background:#f4f4f4;padding:1rem;border-radius:6px;white-space:pre-wrap;word-break:break-all'>"+
		html.EscapeString(string(pretty))+"</pre></body>")
}

func (s *grpStub) userinfo(w http.ResponseWriter, r *http.Request) {
	t, ok := s.bearer(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		stubErr(w, http.StatusUnauthorized, "invalid_token", "missing, unknown, expired or revoked bearer token")
		return
	}
	stubJSON(w, http.StatusOK, s.claimsFor(t.sub))
}

func (s *grpStub) jwks(w http.ResponseWriter, r *http.Request) {
	pub := s.key.PublicKey
	e := make([]byte, 4)
	binary.BigEndian.PutUint32(e, uint32(pub.E))
	stubJSON(w, http.StatusOK, map[string]any{"keys": []map[string]any{{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": stubKid,
		"n":   stubB64(pub.N.Bytes()),
		"e":   stubB64(stubTrimZero(e)),
	}}})
}

func (s *grpStub) introspect(w http.ResponseWriter, r *http.Request) {
	tok := r.Form.Get("token")
	s.mu.Lock()
	t, ok := s.access[tok]
	if !ok {
		t, ok = s.refresh[tok]
	}
	s.mu.Unlock()
	active := ok && !t.revoked && time.Now().Before(t.expires)
	resp := map[string]any{"active": active}
	if active {
		resp["sub"] = t.sub
		resp["scope"] = t.scope
		resp["client_id"] = t.clientID
		resp["token_type"] = "bearer"
		resp["exp"] = t.expires.Unix()
	}
	stubJSON(w, http.StatusOK, resp)
}

func (s *grpStub) revoke(w http.ResponseWriter, r *http.Request) {
	tok := r.Form.Get("token")
	s.mu.Lock()
	if t, ok := s.access[tok]; ok {
		t.revoked = true
		s.access[tok] = t
	}
	if t, ok := s.refresh[tok]; ok {
		t.revoked = true
		s.refresh[tok] = t
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK) // RFC 7009: 200 whether or not the token was known
}

// bearer returns the live (unrevoked, unexpired) token named by the Authorization header.
func (s *grpStub) bearer(r *http.Request) (stubToken, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return stubToken{}, false
	}
	s.mu.Lock()
	t, ok := s.access[strings.TrimSpace(h[len("bearer "):])]
	s.mu.Unlock()
	return t, ok && !t.revoked && time.Now().Before(t.expires)
}

func (s *grpStub) claimsFor(sub string) map[string]any {
	out := map[string]any{}
	for k, v := range stubClaims {
		out[k] = v
	}
	out["sub"] = sub
	return out
}

// idToken builds an RS256-signed OIDC id_token whose signature verifies against the served JWKS.
func (s *grpStub) idToken(sub, aud, nonce string) string {
	now := time.Now()
	payload := s.claimsFor(sub)
	payload["iss"] = stubIssuer
	payload["aud"] = aud
	payload["iat"] = now.Unix()
	payload["exp"] = now.Add(stubTokenTTL).Unix()
	if nonce != "" {
		payload["nonce"] = nonce
	}
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": stubKid})
	body, _ := json.Marshal(payload)
	signingInput := stubB64(header) + "." + stubB64(body)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		log.Printf("sign id_token: %v", err)
		return ""
	}
	return signingInput + "." + stubB64(sig)
}

func stubVerifyPKCE(challenge, method, verifier string) bool {
	if verifier == "" {
		return false
	}
	if strings.EqualFold(method, "plain") || method == "" {
		return verifier == challenge
	}
	sum := sha256.Sum256([]byte(verifier))
	return stubB64(sum[:]) == challenge
}

func stubRandom() string {
	b := make([]byte, 32)
	rand.Read(b)
	return stubB64(b)
}

func stubB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func stubTrimZero(b []byte) []byte {
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	return b
}

// gsb answers VW's GSB web-service calls. Guided functions use it for online operations; component
// protection posts a PinService ProcessPin (a DMS XMLMSG). The virtual ECU accepts whatever the
// function writes afterwards, so a minimal success envelope is enough for the function to complete
// its "Freischaltung" without VW's backend. A real response body can be added if a flow needs one.
func (s *grpStub) gsb(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Header/><soap:Body/></soap:Envelope>`)
}

func stubJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func stubErr(w http.ResponseWriter, status int, code, desc string) {
	stubJSON(w, status, map[string]any{"error": code, "error_description": desc})
}
