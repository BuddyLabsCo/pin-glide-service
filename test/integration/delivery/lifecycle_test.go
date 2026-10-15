//go:build integration

package delivery_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/delivery"
	"github.com/coder/websocket"
)

// sendAbandonedUpload issues one upload whose caller can abandon the request
// before the service answers it, so a test can observe work that outlives its
// caller.
func (e *environment) sendAbandonedUpload(credential, body string) (context.CancelFunc, <-chan uploadAttempt) {
	ctx, abandon := context.WithCancel(context.Background())
	answered := make(chan uploadAttempt, 1)
	go func() {
		defer close(answered)

		request, err := http.NewRequestWithContext(ctx, http.MethodPost,
			e.service.BaseURL+"/v1/messages", strings.NewReader(body))
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
	return abandon, answered
}

// waitForStatus waits until one Delivery identity holds the status a test
// expects, so work that continued without its caller is observed after it
// finished rather than at a moment it happened to be read.
func waitForStatus(t *testing.T, e *environment, senderID, messageID, receiverID, want string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if stored := e.statusOf(t, senderID, messageID, receiverID); stored == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stored Delivery status = %q, want %q",
				e.statusOf(t, senderID, messageID, receiverID), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestBoundedShutdownInterruptsOwnedWork shows a Delivery the process still
// owned when it was asked to stop: shutdown ends the wait the registered work
// was blocked in, joins it and stores interrupted, so the process neither
// leaves registered work behind nor writes ciphertext while it stops.
func TestBoundedShutdownInterruptsOwnedWork(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-shutdown", "openid-shutdown")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Stopped")
	socket := receiverConnected(t, environment, receiverToken)

	const messageID = "message-shutdown"
	answered := environment.sendUpload(sender.Uploader.Token, uploadBody(messageID, entryOf(receiverID, pairingID)))
	// The Receiver holds the delivery without acknowledging it, so the service
	// is waiting for a Receipt when the process is asked to stop.
	frame := readDelivery(t, socket)
	if frame.MessageID != messageID {
		t.Fatalf("the forwarded frame = %+v, want %s", frame, messageID)
	}

	if err := environment.service.Stop(); err != nil {
		t.Fatalf("stop the composed service: %v", err)
	}

	status, response, payload := awaitUpload(t, answered)
	if status != http.StatusMultiStatus {
		t.Fatalf("upload status = %d: %s", status, payload)
	}
	expectResult(t, response, 0, receiverID, "interrupted", "interrupted")
	if stored := environment.statusOf(t, sender.SenderID, messageID, receiverID); stored != "interrupted" {
		t.Errorf("the stored Delivery status = %q, want interrupted", stored)
	}

	// The delivery connection the Receiver held ends with the process, with the
	// going-away meaning rather than a Delivery's own outcome.
	closed, cancelClosed := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClosed()
	if _, _, err := socket.Read(closed); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Errorf("the delivery connection ended with %v, want the going-away meaning", err)
	}
}

// TestUploadOutlivesItsDisconnectedCaller shows registered work continuing
// after its HTTP caller is gone: the caller abandons the request while the
// service waits for the Receipt, and the Delivery still reaches and stores its
// terminal outcome instead of being abandoned or sent a second time.
func TestUploadOutlivesItsDisconnectedCaller(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-abandoned", "openid-abandoned")
	receiverID, pairingID, receiverToken := environment.paired(t, sender, "Abandoned")
	socket := receiverConnected(t, environment, receiverToken)

	const messageID = "message-abandoned"
	abandon, answered := environment.sendAbandonedUpload(sender.Uploader.Token,
		uploadBody(messageID, entryOf(receiverID, pairingID)))
	frame := readDelivery(t, socket)

	abandon()
	attempt := <-answered
	if attempt.err == nil {
		t.Fatalf("the abandoned caller was answered with %d: %s", attempt.status, attempt.payload)
	}

	// The abandoned caller did not cancel the work it had admitted.
	receipt(t, socket, frame)
	waitForStatus(t, environment, sender.SenderID, messageID, receiverID, "received")
	expectNoDelivery(t, socket)
}

// TestShutdownRefusesWorkItCannotJoin shows a submission arriving after the
// process stopped taking Delivery work: the service reports the uncertainty it
// can no longer resolve instead of registering work its join does not cover.
func TestShutdownRefusesWorkItCannotJoin(t *testing.T) {
	environment := start(t)
	sender := environment.sender(t, "code-stopped", "openid-stopped")
	receiverID, pairingID, _ := environment.paired(t, sender, "Stopped")

	if err := environment.delivery.Shutdown(t.Context()); err != nil {
		t.Fatalf("shut down the controlled service: %v", err)
	}

	outcome := <-environment.directUpload(sender.Uploader.Token,
		directRequest("message-stopped", receiverID, pairingID))
	if !errors.Is(outcome.err, domain.ErrStatusUnavailable) {
		t.Errorf("the submission after shutdown failed with %v, want %v",
			outcome.err, domain.ErrStatusUnavailable)
	}
	environment.channel.expectNoWrite(t)
}
