//go:build integration

// Package pairing_test exercises Pairing Session creation, Sender
// authorization and the restricted pairing channel the way a client
// experiences them: HTTP and real WSS requests against the composed service, a
// disposable real PostgreSQL holding the shipped schema, and a fake WeChat
// boundary.
package pairing_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	wechatfake "github.com/BuddyLabsCo/pin-glide-service/test/fakes/wechat"
	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
	"github.com/coder/websocket"
)

// environment is one composed service with its disposable database, faked
// WeChat boundary and registered Senders.
type environment struct {
	service *harness.Service
	// databaseURL is the disposable database the composed service uses, which a
	// test restarting the service composes the next process against.
	databaseURL string
	provider    *wechatfake.Fake
	pool        *sql.DB
	// postgres is the disposable instance itself, which a test stopping the
	// database deliberately owns before harness cleanup runs.
	postgres *harness.Postgres
}

// start composes the service against the shipped schema.
func start(t *testing.T) *environment {
	t.Helper()

	postgres := harness.StartPostgres(t)
	postgres.ApplySchema(t, "pin_glide")

	pool, err := db.Open(context.Background(), postgres.DSN)
	if err != nil {
		t.Fatalf("open disposable database: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	provider := wechatfake.New()
	return &environment{
		service:     harness.StartService(t, postgres.DSN, provider),
		databaseURL: postgres.DSN,
		provider:    provider,
		pool:        pool,
		postgres:    postgres,
	}
}

// stopDatabase removes the disposable instance the composed service uses, so a
// test observes how the service reports storage that stopped answering.
func (e *environment) stopDatabase(t *testing.T) {
	t.Helper()

	if err := e.postgres.Stop(); err != nil {
		t.Fatalf("stop disposable database: %v", err)
	}
}

// sender logs in one installation and returns the credential bundle its
// holder uses for management operations.
func (e *environment) sender(t *testing.T, code, openID string) bundle {
	t.Helper()
	e.provider.Accept(code, identity.WeChatIdentity{AppID: "wx-app", OpenID: openID})

	status, payload := e.post(t, "/v1/auth/wechat", `{"wechatCode":"`+code+`"}`, "")
	if status != http.StatusOK {
		t.Fatalf("login status = %d: %s", status, payload)
	}
	var issued bundle
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode credential bundle: %v: %s", err, payload)
	}
	return issued
}

// post issues one authenticated JSON request and returns its status and body.
func (e *environment) post(t *testing.T, path, body, credential string) (int, []byte) {
	t.Helper()

	return e.request(t, http.MethodPost, path, body, credential)
}

// request issues one JSON request with an optional bearer credential.
func (e *environment) request(t *testing.T, method, path, body, credential string) (int, []byte) {
	t.Helper()

	request, err := http.NewRequest(method, e.service.BaseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	response, err := e.service.Client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return response.StatusCode, payload
}

// exec runs one statement against the database the composed service uses.
func (e *environment) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	if _, err := e.pool.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

// createSession opens one session and returns its restricted credential.
func (e *environment) createSession(t *testing.T, mode, receiverID, credential string) (sessionView, string) {
	t.Helper()

	body := `{"mode":"` + mode + `"}`
	if receiverID != "" {
		body = `{"mode":"` + mode + `","receiverId":"` + receiverID + `"}`
	}
	status, payload := e.post(t, "/v1/pairing-sessions", body, credential)
	if status != http.StatusCreated {
		t.Fatalf("create %s session status = %d, want 201: %s", mode, status, payload)
	}

	var created struct {
		Session               sessionView `json:"session"`
		PairingToken          string      `json:"pairingToken"`
		PairingTokenExpiresAt time.Time   `json:"pairingTokenExpiresAt"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode created session: %v: %s", err, payload)
	}
	if created.Session.SessionID == "" || created.PairingToken == "" {
		t.Fatalf("created session is incomplete: %s", payload)
	}
	if created.PairingTokenExpiresAt.IsZero() {
		t.Fatalf("created session has no credential expiry: %s", payload)
	}
	return created.Session, created.PairingToken
}

// createRecovery opens one recovery session for an existing Receiver. Creation
// takes no credential, as it does for a new Receiver: the fresh owner approval
// belongs to the claim.
func (e *environment) createRecovery(t *testing.T, receiverID string) (sessionView, string) {
	t.Helper()

	status, payload := e.post(t, "/v1/pairing-sessions",
		`{"mode":"recover_receiver","receiverId":"`+receiverID+`"}`, "")
	if status != http.StatusCreated {
		t.Fatalf("create recovery session status = %d, want 201: %s", status, payload)
	}
	var created struct {
		Session      sessionView `json:"session"`
		PairingToken string      `json:"pairingToken"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode created recovery session: %v: %s", err, payload)
	}
	if created.Session.Mode != "recover_receiver" || created.PairingToken == "" {
		t.Fatalf("created recovery session is incomplete: %s", payload)
	}
	return created.Session, created.PairingToken
}

// insertSenderSession writes the session row that add_sender creation will
// write once Receiver credential issuance exists, so the replacement path of an
// already associated Receiver is reachable before that slice lands.
// shortcut: the fixture restates insertSession's row shape, so replace it with
// the real add_sender create path when that slice lands.
func (e *environment) insertSenderSession(t *testing.T, receiverID string) string {
	t.Helper()

	const sessionID = "session-add-sender-fixture"
	// token_digest is unique per session; nothing holds this value.
	e.exec(t, `INSERT INTO pin_glide.pairing_sessions
			(id, mode, receiver_id, state, token_digest, expires_at)
		VALUES ($1, 'add_sender', $2, 'awaiting_authorization', $3, now() + interval '5 minutes')`,
		sessionID, receiverID, strings.Repeat("a", 64))
	return sessionID
}

// authorize claims one session for a Sender and returns the resulting view.
func (e *environment) authorize(t *testing.T, sessionID, alias, credential string) (int, sessionView, []byte) {
	t.Helper()

	body := `{}`
	if alias != "" {
		body = `{"alias":"` + alias + `"}`
	}
	return e.claim(t, sessionID, body, credential)
}

// claim posts one authorize body and decodes the session it returns.
func (e *environment) claim(t *testing.T, sessionID, body, credential string) (int, sessionView, []byte) {
	t.Helper()

	status, payload := e.post(t, "/v1/pairing-sessions/"+sessionID+"/authorize", body, credential)
	if status != http.StatusOK {
		return status, sessionView{}, payload
	}
	var view sessionView
	if err := json.Unmarshal(payload, &view); err != nil {
		t.Fatalf("decode authorized session: %v: %s", err, payload)
	}
	return status, view, payload
}

// dial opens one authenticated pairing subscription.
func (e *environment) dial(t *testing.T, sessionID, credential string) *websocket.Conn {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.service.BaseURL, "http") + "/v1/pairing-sessions/" + sessionID + "/events"
	socket, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + credential}},
		Subprotocols: []string{"pin-glide.v1"},
	})
	if err != nil {
		t.Fatalf("dial pairing channel: %v", err)
	}
	socket.SetReadLimit(64 * 1024)
	t.Cleanup(func() { _ = socket.CloseNow() })
	return socket
}

// dialDelivery opens one R-authenticated delivery subscription on one composed
// service. A rejected handshake returns the problem response of the refusal.
func dialDelivery(t *testing.T, service *harness.Service, credential string) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(service.BaseURL, "http") + "/v1/receiver/events"
	socket, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + credential}},
		Subprotocols: []string{"pin-glide.v1"},
	})
	if err != nil {
		return nil, response, err
	}
	socket.SetReadLimit(64 * 1024)
	return socket, response, nil
}

// connectedDelivery requires one credential to open the delivery channel of one
// composed service.
func connectedDelivery(t *testing.T, service *harness.Service, credential string) *websocket.Conn {
	t.Helper()

	socket, response, err := dialDelivery(t, service, credential)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("connect Receiver: %v (status %d)", err, status)
	}
	t.Cleanup(func() { _ = socket.CloseNow() })
	return socket
}

// readFrame reads one application frame, failing the test when it does not
// arrive inside the bound.
func readFrame(t *testing.T, socket *websocket.Conn, into any) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	messageType, data, err := socket.Read(ctx)
	if err != nil {
		t.Fatalf("read pairing frame: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("pairing frame type = %v, want text", messageType)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("decode pairing frame: %v: %s", err, data)
	}
}

// problem is the decoded whole-request failure payload.
type problem struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
}

// bundle is the decoded login success payload.
type bundle struct {
	SenderID    string `json:"senderId"`
	UserSession struct {
		Token string `json:"token"`
	} `json:"userSession"`
	Uploader struct {
		Token string `json:"token"`
	} `json:"uploader"`
}

// sessionView is the decoded PairingSessionView.
type sessionView struct {
	SessionID         string    `json:"sessionId"`
	Mode              string    `json:"mode"`
	ReceiverID        string    `json:"receiverId"`
	State             string    `json:"state"`
	ExpiresAt         time.Time `json:"expiresAt"`
	SenderConfirmed   bool      `json:"senderConfirmed"`
	ReceiverConfirmed bool      `json:"receiverConfirmed"`
	SenderID          string    `json:"senderId"`
	PairingID         string    `json:"pairingId"`
}

// The normal new-receiver flow: anonymous creation allocates both IDs, returns
// the restricted credential only here, and the first authenticated Sender
// claim fixes ownership and allocates one pending Pairing ID. Repeating the
// same Sender's claim is idempotent; another Sender is refused.
func TestPairingSessionCreationAndSenderClaim(t *testing.T) {
	environment := start(t)
	senderA := environment.sender(t, "code-a", "openid-a")
	senderB := environment.sender(t, "code-b", "openid-b")

	created, token := environment.createSession(t, "new_receiver", "", "")
	if created.State != "awaiting_authorization" || created.SenderID != "" || created.PairingID != "" {
		t.Fatalf("created session = %+v, want an unclaimed awaiting_authorization session", created)
	}
	if created.ReceiverID == "" {
		t.Fatal("new_receiver did not allocate a Receiver")
	}
	if lifetime := created.ExpiresAt.Sub(time.Now().UTC()); lifetime > 5*time.Minute || lifetime < 4*time.Minute {
		t.Errorf("session lifetime = %v, want about five minutes", lifetime)
	}

	status, claimed, payload := environment.authorize(t, created.SessionID, "Phone", senderA.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("claim status = %d, want 200: %s", status, payload)
	}
	if claimed.State != "awaiting_confirmation" || claimed.SenderID != senderA.SenderID || claimed.PairingID == "" {
		t.Fatalf("claimed session = %+v, want a claimed awaiting_confirmation session with a Pairing ID", claimed)
	}
	if claimed.ReceiverID != created.ReceiverID {
		t.Errorf("claimed receiver = %q, want %q", claimed.ReceiverID, created.ReceiverID)
	}

	status, repeated, payload := environment.authorize(t, created.SessionID, "", senderA.UserSession.Token)
	if status != http.StatusOK || repeated.PairingID != claimed.PairingID {
		t.Fatalf("repeated claim = %d %+v, want the same session: %s", status, repeated, payload)
	}
	if count := environment.count(t, `SELECT count(*) FROM pin_glide.pairing_pairings WHERE sender_id = $1`, senderA.SenderID); count != 1 {
		t.Errorf("repeated claim allocated %d associations, want 1", count)
	}

	status, _, payload = environment.authorize(t, created.SessionID, "", senderB.UserSession.Token)
	if status != http.StatusConflict {
		t.Fatalf("foreign claim status = %d, want 409: %s", status, payload)
	}
	if decoded := decodeProblem(t, payload); decoded.Code != "pairing_session_claimed" {
		t.Errorf("foreign claim code = %q, want pairing_session_claimed", decoded.Code)
	}

	// The restricted credential opens the channel for the Receiver side and
	// never authorizes management.
	if status, payload := environment.post(t, "/v1/pairing-sessions/"+created.SessionID+"/authorize", `{}`, token); status != http.StatusUnauthorized {
		t.Errorf("restricted credential authorized a claim: %d %s", status, payload)
	}

	// A lapsed session reports its own outcome to the Sender that claimed it:
	// repeating the claim never extends the lifetime.
	environment.exec(t, `UPDATE pin_glide.pairing_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, created.SessionID)
	status, _, payload = environment.authorize(t, created.SessionID, "", senderA.UserSession.Token)
	if status != http.StatusGone {
		t.Fatalf("claim of a lapsed session = %d, want 410: %s", status, payload)
	}
	if decoded := decodeProblem(t, payload); decoded.Code != "pairing_session_expired" {
		t.Errorf("lapsed claim code = %q, want pairing_session_expired", decoded.Code)
	}
}

// relayFrame builds one pairing_relay frame carrying already padded-Base64
// handshake bytes.
func relayFrame(sessionID, payload string) string {
	return `{"version":1,"type":"pairing_relay","sessionId":"` + sessionID + `","payload":"` + payload + `"}`
}

// writeFrame sends one application frame.
func writeFrame(t *testing.T, socket *websocket.Conn, frame string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := socket.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("write pairing frame: %v", err)
	}
}

// decodeProblem decodes one whole-request failure payload.
func decodeProblem(t *testing.T, payload []byte) problem {
	t.Helper()

	var decoded problem
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode problem: %v: %s", err, payload)
	}
	return decoded
}

// count returns one row count from the database the composed service uses.
func (e *environment) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()

	var value int64
	if err := e.pool.QueryRowContext(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}
