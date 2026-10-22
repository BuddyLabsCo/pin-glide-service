import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import http from 'node:http';
import test from 'node:test';

import { TARGET } from './target.mjs';
import { openWebSocket } from './websocket.mjs';

function serverFrame(opcode, payload = Buffer.alloc(0)) {
  const body = Buffer.from(payload);
  const header = body.length < 126
    ? Buffer.from([0x80 | opcode, body.length])
    : Buffer.from([0x80 | opcode, 126, body.length >> 8, body.length & 0xff]);
  return Buffer.concat([header, body]);
}

test('the WebSocket peer presents bearer authority and answers service pings', async t => {
  let authorization = '';
  let protocol = '';
  let pong = '';
  const server = http.createServer();
  server.on('upgrade', (request, socket) => {
    authorization = request.headers.authorization || '';
    protocol = request.headers['sec-websocket-protocol'] || '';
    const accept = createHash('sha1')
      .update(`${request.headers['sec-websocket-key']}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`)
      .digest('base64');
    socket.write([
      'HTTP/1.1 101 Switching Protocols',
      'Upgrade: websocket',
      'Connection: Upgrade',
      `Sec-WebSocket-Accept: ${accept}`,
      'Sec-WebSocket-Protocol: pin-glide.v1',
      '',
      '',
    ].join('\r\n'));
    socket.write(serverFrame(1, Buffer.from('{"type":"pairing_state"}'))
      .toString('binary'), 'binary');
    socket.write(serverFrame(9, Buffer.from('probe')).toString('binary'), 'binary');
    socket.once('data', data => {
      const masked = (data[1] & 0x80) !== 0;
      const length = data[1] & 0x7f;
      const offset = 2 + (length === 126 ? 2 : 0) + (masked ? 4 : 0);
      const payload = Buffer.from(data.subarray(offset, offset + length));
      if (masked) {
        const mask = data.subarray(offset - 4, offset);
        for (let index = 0; index < payload.length; index++) payload[index] ^= mask[index % 4];
      }
      pong = payload.toString();
    });
    t.after(() => socket.destroy());
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const origin = `ws://127.0.0.1:${server.address().port}`;
  const localTarget = { ...TARGET, wsOrigin: origin };

  const socket = await openWebSocket({
    origin,
    path: '/v1/receiver/events',
    token: 'receiver-secret',
    target: localTarget,
  });
  t.after(() => socket.close());
  const message = await socket.next();
  assert.deepEqual(message.json, { type: 'pairing_state' });

  for (let attempt = 0; attempt < 50 && pong !== 'probe'; attempt++) {
    await new Promise(resolve => setTimeout(resolve, 10));
  }
  assert.equal(authorization, 'Bearer receiver-secret');
  assert.equal(protocol, 'pin-glide.v1');
  assert.equal(pong, 'probe');
});

test('the WebSocket peer can leave a service Ping unanswered', async t => {
  let pong = false;
  const server = http.createServer();
  server.on('upgrade', (request, socket) => {
    const accept = createHash('sha1')
      .update(`${request.headers['sec-websocket-key']}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`)
      .digest('base64');
    socket.write([
      'HTTP/1.1 101 Switching Protocols',
      'Upgrade: websocket',
      'Connection: Upgrade',
      `Sec-WebSocket-Accept: ${accept}`,
      'Sec-WebSocket-Protocol: pin-glide.v1',
      '',
      '',
    ].join('\r\n'));
    socket.write(serverFrame(9, Buffer.from('probe')));
    socket.on('data', data => {
      if ((data[0] & 0x0f) === 10) pong = true;
    });
    t.after(() => socket.destroy());
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const origin = `ws://127.0.0.1:${server.address().port}`;

  const socket = await openWebSocket({
    origin,
    path: '/v1/receiver/events',
    token: 'receiver-secret',
    answerPings: false,
    target: { ...TARGET, wsOrigin: origin },
  });
  t.after(() => socket.close());
  await new Promise(resolve => setTimeout(resolve, 100));
  assert.equal(socket.pings, 1);
  assert.equal(pong, false);
});
