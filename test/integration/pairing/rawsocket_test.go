//go:build integration

package pairing_test

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The WebSocket opcodes and close meanings a conformance test writes and reads.
const (
	rawText   = 0x1
	rawBinary = 0x2
	rawClose  = 0x8
	rawPing   = 0x9
	rawPong   = 0xa
)

// rawSocket is a WebSocket client of the transport rather than of a library.
// It exists to send the frames a conforming client library refuses to send -
// invalid text encoding above all - and to read the close meaning the service
// answers with, which is what the published description promises.
type rawSocket struct {
	connection net.Conn
	reader     *bufio.Reader
}

// dialRawSocket completes a WebSocket upgrade by hand, so a test controls every
// byte of the frames that follow.
func dialRawSocket(t *testing.T, baseURL, path, credential, subprotocol string) *rawSocket {
	t.Helper()

	socket, refusal := upgradeRawSocket(t, baseURL, path, credential, subprotocol)
	if socket == nil {
		refuseUpgrade(t, refusal, http.StatusSwitchingProtocols, "")
	}
	return socket
}

// upgradeRawSocket completes one WebSocket upgrade by hand and reports either
// the served connection or the service's refusal of it, so a test can assert
// the problem a refused upgrade answered with.
func upgradeRawSocket(t *testing.T, baseURL, path, credential, subprotocol string) (*rawSocket, *http.Response) {
	t.Helper()

	connection, err := net.Dial("tcp", strings.TrimPrefix(baseURL, "http://"))
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	reader := bufio.NewReader(connection)

	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	request := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + strings.TrimPrefix(baseURL, "http://") + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n"
	if credential != "" {
		request += "Authorization: Bearer " + credential + "\r\n"
	}
	if subprotocol != "" {
		request += "Sec-WebSocket-Protocol: " + subprotocol + "\r\n"
	}
	request += "\r\n"

	if err := connection.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set handshake deadline: %v", err)
	}
	if _, err := connection.Write([]byte(request)); err != nil {
		_ = connection.Close()
		t.Fatalf("write handshake of %s: %v", path, err)
	}

	status, head, body := readHandshake(t, reader)
	if status != http.StatusSwitchingProtocols {
		_ = connection.Close()
		return nil, &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{},
		}
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("clear handshake deadline: %v", err)
	}

	// The server may have switched to permessage-deflate if the test offered
	// it, so a frame-reading test asserts on the uncompressed negotiation a
	// conformance client uses.
	if strings.Contains(head, "Sec-WebSocket-Extensions") {
		_ = connection.Close()
		t.Fatalf("upgrade selected an extension this test cannot decode: %s", head)
	}

	socket := &rawSocket{connection: connection, reader: reader}
	t.Cleanup(func() { _ = connection.Close() })
	return socket, nil
}

// readHandshake reads one HTTP response head and returns its status, its header
// text and whatever body followed it, so a refusal and a successful upgrade
// share one reader.
func readHandshake(t *testing.T, reader *bufio.Reader) (int, string, string) {
	t.Helper()

	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read response status line: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(statusLine), " ")
	if len(parts) < 2 {
		t.Fatalf("response status line = %q", statusLine)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("response status line = %q: %v", statusLine, err)
	}

	head := statusLine
	contentLength := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read response headers: %v", err)
		}
		head += line
		if line == "\r\n" || line == "\n" {
			break
		}
		if name, value, found := strings.Cut(line, ":"); found && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			contentLength, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				t.Fatalf("response Content-Length = %q: %v", value, err)
			}
		}
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatalf("read response body of %d bytes: %v", contentLength, err)
	}
	return status, head, string(body)
}

// writeFrame writes one masked client frame, the way RFC 6455 requires a client
// to send it.
func (s *rawSocket) writeFrame(t *testing.T, opcode byte, payload []byte) {
	t.Helper()

	if err := s.connection.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set write deadline: %v", err)
	}
	key := []byte{0x11, 0x22, 0x33, 0x44}
	masked := make([]byte, len(payload))
	for index, value := range payload {
		masked[index] = value ^ key[index%len(key)]
	}

	frame := []byte{0x80 | opcode}
	switch size := len(masked); {
	case size < 126:
		frame = append(frame, byte(0x80|size))
	case size <= 0xffff:
		frame = append(frame, 0x80|126, byte(size>>8), byte(size))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(size))
	}
	frame = append(frame, key...)
	frame = append(frame, masked...)

	if _, err := s.connection.Write(frame); err != nil {
		t.Fatalf("write frame %#x: %v", opcode, err)
	}
}

// read reads one unmasked server frame and returns its opcode with its payload,
// so a test sees the raw close meaning instead of a client library's
// interpretation of it.
func (s *rawSocket) read() (byte, []byte, error) {
	if err := s.connection.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return 0, nil, err
	}
	header := make([]byte, 2)
	if _, err := io.ReadFull(s.reader, header); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0f
	length := int(header[1] & 0x7f)
	switch {
	case length == 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(s.reader, extended); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(extended))
	case length == 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(s.reader, extended); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(extended))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(s.reader, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}

// readFrame reads one unmasked server frame, failing the test when the
// connection ends or the frame cannot be read.
func (s *rawSocket) readFrame(t *testing.T) (byte, []byte) {
	t.Helper()

	opcode, payload, err := s.read()
	if err != nil {
		t.Fatalf("read service frame: %v", err)
	}
	return opcode, payload
}

// awaitClose reads service frames until the connection ends, so a caller that
// waits for a close ignores the liveness traffic the service sends meanwhile.
func (s *rawSocket) awaitClose() (int, string, error) {
	for {
		opcode, payload, err := s.read()
		if err != nil {
			return 0, "", err
		}
		if opcode == rawPing || opcode == rawPong {
			continue
		}
		if opcode != rawClose {
			return 0, "", fmt.Errorf("service frame opcode = %#x, want a close", opcode)
		}
		if len(payload) < 2 {
			return 0, "", errors.New("close frame carries no status")
		}
		return int(binary.BigEndian.Uint16(payload[:2])), string(payload[2:]), nil
	}
}

// requireCloseFrame requires the service to end the connection with the close
// code want, and returns the reason it sent with it.
func (s *rawSocket) requireCloseFrame(t *testing.T, want int) string {
	t.Helper()

	code, reason, err := s.awaitClose()
	if err != nil {
		t.Fatalf("read service close: %v", err)
	}
	if code != want {
		t.Errorf("close code = %d, want %d", code, want)
	}
	return reason
}

// receiveMessage requires the service to send one application message of the
// wanted type and decodes it into into.
func (s *rawSocket) receiveMessage(t *testing.T, want byte, into any) {
	t.Helper()

	opcode, payload := s.readFrame(t)
	if opcode != want {
		t.Fatalf("service frame opcode = %#x, want %#x: %s", opcode, want, payload)
	}
	if err := json.Unmarshal(payload, into); err != nil {
		t.Fatalf("decode service message: %v: %s", err, payload)
	}
}

// refuseUpgrade requires a service answer to be a problem-detail refusal with
// the status and code a client matches on.
func refuseUpgrade(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()

	if response == nil {
		t.Fatalf("upgrade refused with no response, want %d %s", status, code)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != status {
		t.Fatalf("upgrade status = %d, want %d", response.StatusCode, status)
	}
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read refusal body: %v", err)
	}
	var problem struct {
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &problem); err != nil {
		t.Fatalf("decode refusal %s: %v", payload, err)
	}
	if problem.Code != code {
		t.Errorf("upgrade refusal code = %q, want %q", problem.Code, code)
	}
	if problem.Type != "/problems/"+code {
		t.Errorf("upgrade refusal type = %q, want /problems/%s", problem.Type, code)
	}
}
