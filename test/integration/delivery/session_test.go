//go:build integration

// Package delivery_test exercises Delivery admission, live WSS forwarding,
// Receipt correlation and retained results the way clients experience them:
// HTTP and real WSS requests against the composed service, a disposable real
// PostgreSQL holding the shipped schema, and a fake WeChat boundary.
package delivery_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/db"
	domain "github.com/BuddyLabsCo/pin-glide-service/internal/delivery"
	"github.com/BuddyLabsCo/pin-glide-service/internal/identity"
	"github.com/BuddyLabsCo/pin-glide-service/internal/protocol"
	wechatfake "github.com/BuddyLabsCo/pin-glide-service/test/fakes/wechat"
	"github.com/BuddyLabsCo/pin-glide-service/test/integration/harness"
	"github.com/coder/websocket"
)

// The synthetic ciphertext of every test: a 12-byte nonce and 17 synthetic
// bytes that the service accepts as the adopted profile without decrypting
// anything. No test asserts a working vector, and none of these bytes is a
// nonce a client may reuse.
const (
	syntheticNonce      = "AAAAAAAAAAAAAAAA"
	syntheticCiphertext = "AQIDBAUGBwgJCgsMDQ4PEBESExQ="
)

// environment is one composed service with its disposable database, faked
// WeChat boundary and registered Senders.
type environment struct {
	service     *harness.Service
	provider    *wechatfake.Fake
	pool        *sql.DB
	postgres    *harness.Postgres
	databaseURL string
	// channel is the live transport a test decides about, and delivery is the
	// same domain service composed on it instead of on a Receiver connection.
	// Both share this environment's database and credentials, so a test can ask
	// for a write outcome a real Receiver cannot be made to produce.
	channel  *controlledChannel
	delivery *domain.Service
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
	channel := newControlledChannel()
	return &environment{
		service:     harness.StartService(t, postgres.DSN, provider, nil),
		provider:    provider,
		pool:        pool,
		postgres:    postgres,
		databaseURL: postgres.DSN,
		channel:     channel,
		delivery: domain.NewService(pool, channel,
			domain.NewAuthenticator(identity.NewService(pool, provider))),
	}
}

// bundle is the decoded Sender credential bundle.
type bundle struct {
	SenderID    string `json:"senderId"`
	UserSession struct {
		Token string `json:"token"`
	} `json:"userSession"`
	Uploader struct {
		Token string `json:"token"`
	} `json:"uploader"`
}

// restart replaces the running composed service with a new process over the
// same database, which is what a restart is here: live work is gone while the
// retained identities, outcomes and credentials remain.
func (e *environment) restart(t *testing.T) {
	t.Helper()

	if err := e.service.Stop(); err != nil {
		t.Fatalf("stop the composed service: %v", err)
	}
	e.service = harness.StartService(t, e.databaseURL, e.provider, nil)
}

// problem is the decoded RFC 9457 problem detail.
type problem struct {
	Type      string `json:"type"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	RequestID string `json:"requestId"`
	Errors    []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
	} `json:"errors"`
}

// uploadResult is one decoded 207 entry result.
type uploadResult struct {
	Index      int    `json:"index"`
	ReceiverID string `json:"receiverId"`
	Status     string `json:"status"`
	Error      *struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"error"`
}

// uploadResponse is the decoded 207 payload.
type uploadResponse struct {
	MessageID string         `json:"messageId"`
	RequestID string         `json:"requestId"`
	Results   []uploadResult `json:"results"`
}

// deliveryView is one decoded DeliveryView of the metadata query.
type deliveryView struct {
	ReceiverID  string     `json:"receiverId"`
	PairingID   string     `json:"pairingId"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	CompletedAt *time.Time `json:"completedAt"`
	Error       *struct {
		Code string `json:"code"`
	} `json:"error"`
}

// sender logs in one installation and returns its credential bundle.
func (e *environment) sender(t *testing.T, code, openID string) bundle {
	t.Helper()
	e.provider.Accept(code, identity.WeChatIdentity{AppID: "wx-app", OpenID: openID})

	status, payload := e.request(t, http.MethodPost, "/v1/auth/wechat", `{"wechatCode":"`+code+`"}`, "")
	if status != http.StatusOK {
		t.Fatalf("login status = %d: %s", status, payload)
	}
	var issued bundle
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode credential bundle: %v: %s", err, payload)
	}
	return issued
}

// paired drives one new_receiver session to an active association with a
// redeemed Receiver credential, which is the forwarding authority a Delivery
// needs.
func (e *environment) paired(t *testing.T, sender bundle, alias string) (receiverID, pairingID, receiverToken string) {
	t.Helper()

	status, payload := e.request(t, http.MethodPost, "/v1/pairing-sessions", `{"mode":"new_receiver"}`, "")
	if status != http.StatusCreated {
		t.Fatalf("create Pairing Session status = %d: %s", status, payload)
	}
	var created struct {
		Session struct {
			SessionID  string `json:"sessionId"`
			ReceiverID string `json:"receiverId"`
		} `json:"session"`
		PairingToken string `json:"pairingToken"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode created session: %v: %s", err, payload)
	}
	sessionID, restricted := created.Session.SessionID, created.PairingToken

	status, payload = e.request(t, http.MethodPost, "/v1/pairing-sessions/"+sessionID+"/authorize",
		`{"alias":"`+alias+`"}`, sender.UserSession.Token)
	if status != http.StatusOK {
		t.Fatalf("claim status = %d: %s", status, payload)
	}
	var claimed struct {
		PairingID string `json:"pairingId"`
	}
	if err := json.Unmarshal(payload, &claimed); err != nil {
		t.Fatalf("decode claimed session: %v: %s", err, payload)
	}

	for _, credential := range []string{sender.UserSession.Token, restricted} {
		status, payload = e.request(t, http.MethodPost, "/v1/pairing-sessions/"+sessionID+"/complete",
			`{"keyConfirmation":"confirmed"}`, credential)
		if status != http.StatusOK {
			t.Fatalf("key-confirmation report status = %d: %s", status, payload)
		}
	}

	status, payload = e.request(t, http.MethodPost, "/v1/pairing-sessions/"+sessionID+"/receiver-token",
		`{"operationId":"op-`+sessionID+`","attempt":0}`, restricted)
	if status != http.StatusOK {
		t.Fatalf("redemption status = %d: %s", status, payload)
	}
	var issued struct {
		ReceiverID    string `json:"receiverId"`
		ReceiverToken string `json:"receiverToken"`
	}
	if err := json.Unmarshal(payload, &issued); err != nil {
		t.Fatalf("decode redeemed credential: %v: %s", err, payload)
	}
	return issued.ReceiverID, claimed.PairingID, issued.ReceiverToken
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

// revoke permanently closes one Receiver registration the way its Account
// owner does, which is the state a later upload must read as unauthorized.
func (e *environment) revoke(t *testing.T, sender bundle, receiverID, openID string) {
	t.Helper()

	code := "code-revoke-" + receiverID
	e.provider.Accept(code, identity.WeChatIdentity{AppID: "wx-app", OpenID: openID})
	status, payload := e.request(t, http.MethodPost, "/v1/devices/"+receiverID+"/revoke",
		`{"wechatCode":"`+code+`"}`, sender.UserSession.Token)
	if status != http.StatusNoContent {
		t.Fatalf("revocation of %s = %d: %s", receiverID, status, payload)
	}
}

// uploadAttempt is one upload request in flight.
type uploadAttempt struct {
	status  int
	payload []byte
	err     error
}

// sendUpload issues one upload and reports its response. A test that has to
// serve the Receipt an upload is waiting for uses this to keep the request in
// flight while the Receiver answers it.
func (e *environment) sendUpload(credential, body string) <-chan uploadAttempt {
	answered := make(chan uploadAttempt, 1)
	go func() {
		defer close(answered)

		request, err := http.NewRequest(http.MethodPost, e.service.BaseURL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			answered <- uploadAttempt{err: err}
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+credential)

		response, err := e.service.Client.Do(request)
		if err != nil {
			answered <- uploadAttempt{err: err}
			return
		}
		defer func() { _ = response.Body.Close() }()

		payload, err := io.ReadAll(response.Body)
		answered <- uploadAttempt{status: response.StatusCode, payload: payload, err: err}
	}()
	return answered
}

// awaitUpload waits for one upload and decodes its 207 payload.
func awaitUpload(t *testing.T, answered <-chan uploadAttempt) (int, uploadResponse, []byte) {
	t.Helper()

	attempt, ok := <-answered
	if !ok {
		t.Fatal("the upload request produced no response")
	}
	if attempt.err != nil {
		t.Fatalf("upload: %v", attempt.err)
	}
	if attempt.status != http.StatusMultiStatus {
		return attempt.status, uploadResponse{}, attempt.payload
	}
	var response uploadResponse
	if err := json.Unmarshal(attempt.payload, &response); err != nil {
		t.Fatalf("decode upload response: %v: %s", err, attempt.payload)
	}
	return attempt.status, response, attempt.payload
}

// upload posts one upload body and decodes its 207 payload.
func (e *environment) upload(t *testing.T, credential, body string) (int, uploadResponse, []byte) {
	t.Helper()

	return awaitUpload(t, e.sendUpload(credential, body))
}

// entryOf renders one entry of the accepted profile for one Pairing. The
// ciphertext is synthetic: the service never decrypts it, so only its syntax
// and length matter here.
func entryOf(receiverID, pairingID string) string {
	return `{"receiverId":"` + receiverID + `","pairingId":"` + pairingID + `","cryptoVersion":1,` +
		`"nonce":"` + syntheticNonce + `","ciphertext":"` + syntheticCiphertext + `"}`
}

// uploadBody renders one upload envelope around the given entries.
func uploadBody(messageID string, entries ...string) string {
	return `{"messageId":"` + messageID + `","items":[` + strings.Join(entries, ",") + `]}`
}

// receiverConnected opens the one delivery connection of a Receiver credential.
func receiverConnected(t *testing.T, e *environment, credential string) *websocket.Conn {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socket, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.service.BaseURL, "http")+"/v1/receiver/events",
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + credential}},
			Subprotocols: []string{protocol.Subprotocol},
		})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("connect Receiver: %v (status %d)", err, status)
	}
	socket.SetReadLimit(64 * 1024)
	t.Cleanup(func() { _ = socket.CloseNow() })
	return socket
}

// readDelivery reads one delivery frame, failing the test when none arrives
// inside the bound.
func readDelivery(t *testing.T, socket *websocket.Conn) protocol.Delivery {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	messageType, data, err := socket.Read(ctx)
	if err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("delivery arrived as message type %v, want text", messageType)
	}
	var frame protocol.Delivery
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("decode delivery: %v: %s", err, data)
	}
	return frame
}

// expectNoDelivery requires one Receiver connection to stay silent, so a test
// shows that a duplicate or retained Delivery was not sent again.
func expectNoDelivery(t *testing.T, socket *websocket.Conn) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	if messageType, data, err := socket.Read(ctx); err == nil {
		t.Fatalf("the Receiver received unexpected %v traffic: %s", messageType, data)
	} else if websocket.CloseStatus(err) != -1 {
		t.Fatalf("the Receiver connection closed unexpectedly: %v", err)
	}
}

// receipt sends the arrival acknowledgement of one delivery frame.
func receipt(t *testing.T, socket *websocket.Conn, frame protocol.Delivery) {
	t.Helper()

	writeFrame(t, socket, fmt.Sprintf(`{"version":1,"type":"receipt","senderId":%q,"messageId":%q}`,
		frame.SenderID, frame.MessageID))
}

// writeFrame sends one application frame.
func writeFrame(t *testing.T, socket *websocket.Conn, frame string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := socket.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// statusOf reads the stored status of one Delivery identity, reporting an
// absent row as the empty string.
func (e *environment) statusOf(t *testing.T, senderID, messageID, receiverID string) string {
	t.Helper()

	var status string
	err := e.pool.QueryRowContext(context.Background(), `
SELECT status FROM pin_glide.delivery_deliveries
WHERE sender_id = $1 AND message_id = $2 AND receiver_id = $3`,
		senderID, messageID, receiverID).Scan(&status)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read stored Delivery status: %v", err)
	}
	return status
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

// deliveriesOf counts the retained Deliveries of one Message, which is what
// shows whether an invocation admitted a Delivery at all.
func (e *environment) deliveriesOf(t *testing.T, senderID, messageID string) int64 {
	t.Helper()

	return e.count(t, `
SELECT count(*) FROM pin_glide.delivery_deliveries
WHERE sender_id = $1 AND message_id = $2`, senderID, messageID)
}

// exec runs one statement against the database the composed service uses.
func (e *environment) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	if _, err := e.pool.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
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
