import { createHash, randomBytes } from 'node:crypto';
import http from 'node:http';
import https from 'node:https';

import { SmokeProblem } from './http.mjs';
import { serverRelativePath } from './path.mjs';
import { TARGET, assertWebSocketOrigin } from './target.mjs';

const websocketGUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';
const maxMessageBytes = 16 * 1024 * 1024;

function maskedFrame(opcode, payload = Buffer.alloc(0)) {
  const body = Buffer.from(payload);
  let header;
  if (body.length < 126) {
    header = Buffer.from([0x80 | opcode, 0x80 | body.length]);
  } else if (body.length <= 0xffff) {
    header = Buffer.alloc(4);
    header[0] = 0x80 | opcode;
    header[1] = 0x80 | 126;
    header.writeUInt16BE(body.length, 2);
  } else {
    header = Buffer.alloc(10);
    header[0] = 0x80 | opcode;
    header[1] = 0x80 | 127;
    header.writeBigUInt64BE(BigInt(body.length), 2);
  }
  const mask = randomBytes(4);
  const masked = Buffer.alloc(body.length);
  for (let index = 0; index < body.length; index++) masked[index] = body[index] ^ mask[index % 4];
  return Buffer.concat([header, mask, masked]);
}

export class WebSocketClosedError extends Error {
  constructor(close) {
    super(`smoke WebSocket closed with ${close.code}${close.reason ? ` ${close.reason}` : ''}`);
    this.name = 'WebSocketClosedError';
    this.code = close.code;
    this.reason = close.reason;
  }
}

class WebSocketPeer {
  constructor(socket, head) {
    this.socket = socket;
    this.leftover = Buffer.alloc(0);
    this.fragments = undefined;
    this.messages = [];
    this.waiters = [];
    this.closeInfo = undefined;
    this.sendClosed = false;
    this.close = this.close.bind(this);
    this.closeEvent = new Promise(resolve => {
      this.resolveClose = resolve;
    });
    socket.setNoDelay(true);
    socket.on('data', data => {
      try {
        this.feed(data);
      } catch (error) {
        this.socket.destroy(error instanceof Error ? error : new Error(String(error)));
      }
    });
    socket.on('error', error => this.finish({ code: 1006, reason: error.message }));
    socket.on('close', () => this.finish({ code: 1006, reason: 'transport closed' }));
    if (head.length > 0) this.feed(head);
  }

  feed(data) {
    this.leftover = this.leftover.length === 0 ? data : Buffer.concat([this.leftover, data]);
    for (;;) {
      if (this.leftover.length < 2) return;
      const first = this.leftover[0];
      const second = this.leftover[1];
      const fin = (first & 0x80) !== 0;
      const rsv = first & 0x70;
      const opcode = first & 0x0f;
      const masked = (second & 0x80) !== 0;
      let length = second & 0x7f;
      let offset = 2;
      if (rsv !== 0 || masked) throw new Error('invalid server WebSocket frame');
      if (length === 126) {
        if (this.leftover.length < 4) return;
        length = this.leftover.readUInt16BE(2);
        offset = 4;
      } else if (length === 127) {
        if (this.leftover.length < 10) return;
        const large = this.leftover.readBigUInt64BE(2);
        if (large > BigInt(maxMessageBytes)) throw new Error('server WebSocket frame is too large');
        length = Number(large);
        offset = 10;
      }
      if (this.leftover.length < offset + length) return;
      const payload = this.leftover.subarray(offset, offset + length);
      this.leftover = this.leftover.subarray(offset + length);
      this.frame(fin, opcode, payload);
    }
  }

  frame(fin, opcode, payload) {
    if (opcode >= 8) {
      if (!fin || payload.length > 125) throw new Error('invalid server WebSocket control frame');
      if (opcode === 8) return this.receiveClose(payload);
      if (opcode === 9) return this.send(10, payload);
      if (opcode === 10) return;
      throw new Error('unknown server WebSocket control frame');
    }
    if (opcode === 0) {
      if (!this.fragments) throw new Error('unexpected WebSocket continuation');
      this.fragments.parts.push(payload);
      this.fragments.size += payload.length;
      if (this.fragments.size > maxMessageBytes) throw new Error('server WebSocket message is too large');
      if (fin) {
        const complete = this.fragments;
        this.fragments = undefined;
        this.deliver(complete.opcode, Buffer.concat(complete.parts));
      }
      return;
    }
    if (opcode !== 1 && opcode !== 2) throw new Error('unknown server WebSocket data frame');
    if (this.fragments) throw new Error('new WebSocket message before continuation');
    if (!fin) {
      this.fragments = { opcode, parts: [payload], size: payload.length };
      return;
    }
    this.deliver(opcode, payload);
  }

  deliver(opcode, payload) {
    const message = {
      binary: opcode === 2,
      data: payload,
      text: opcode === 1 ? payload.toString('utf8') : '',
    };
    if (opcode === 1) {
      try {
        message.json = JSON.parse(message.text);
      } catch {
        message.json = undefined;
      }
    }
    const waiter = this.waiters.shift();
    if (waiter) {
      clearTimeout(waiter.timer);
      waiter.resolve(message);
    } else {
      this.messages.push(message);
    }
  }

  next(timeoutMs = 5_000) {
    if (this.messages.length > 0) return Promise.resolve(this.messages.shift());
    if (this.closeInfo) return Promise.reject(new WebSocketClosedError(this.closeInfo));
    return new Promise((resolve, reject) => {
      const waiter = { resolve, reject, timer: undefined };
      waiter.timer = setTimeout(() => {
        this.waiters = this.waiters.filter(candidate => candidate !== waiter);
        reject(new Error('timed out waiting for a WebSocket message'));
      }, timeoutMs);
      this.waiters.push(waiter);
    });
  }

  sendJSON(value) {
    this.send(1, Buffer.from(JSON.stringify(value)));
  }

  send(opcode, payload) {
    if (this.closeInfo || this.sendClosed) throw new WebSocketClosedError(this.closeInfo || { code: 1006, reason: 'closed' });
    this.socket.write(maskedFrame(opcode, payload));
  }

  receiveClose(payload) {
    let code = 1005;
    let reason = '';
    if (payload.length >= 2) {
      code = payload.readUInt16BE(0);
      reason = payload.subarray(2).toString('utf8');
    }
    if (!this.sendClosed) {
      this.sendClosed = true;
      this.socket.write(maskedFrame(8, payload.length >= 2 ? payload.subarray(0, 2) : Buffer.alloc(0)));
    }
    this.socket.end();
    this.finish({ code, reason });
  }

  finish(close) {
    if (this.closeInfo) return;
    this.closeInfo = close;
    this.resolveClose(close);
    for (const waiter of this.waiters.splice(0)) {
      clearTimeout(waiter.timer);
      waiter.reject(new WebSocketClosedError(close));
    }
  }

  close(code = 1000, reason = '') {
    if (this.closeInfo || this.sendClosed) return this.closeEvent;
    this.sendClosed = true;
    const text = Buffer.from(reason);
    const payload = Buffer.alloc(2 + Math.min(text.length, 123));
    payload.writeUInt16BE(code, 0);
    text.copy(payload, 2, 0, payload.length - 2);
    this.socket.write(maskedFrame(8, payload));
    this.socket.end();
    const timer = setTimeout(() => {
      this.socket.destroy();
      this.finish({ code, reason });
    }, 1_000);
    timer.unref();
    return this.closeEvent;
  }

  // Simulate a Receiver whose transport disappears without a WebSocket Close
  // handshake. The service may still hold its slot for a moment, which lets a
  // smoke run exercise a failed delivery write rather than an orderly offline
  // release.
  abort(reason = 'smoke transport failure') {
    if (this.closeInfo || this.sendClosed) return;
    this.sendClosed = true;
    if (typeof this.socket.resetAndDestroy === 'function') {
      this.socket.resetAndDestroy();
    } else {
      this.socket.destroy();
    }
    this.finish({ code: 1006, reason });
  }

  // Stop reading the transport without closing it. A bounded server write can
  // then fail for backpressure, which is the reversible fault used to exercise
  // send_failed instead of a fake delivery channel.
  pauseReads() {
    this.socket.pause();
  }

  resumeReads() {
    if (!this.closeInfo && !this.sendClosed) this.socket.resume();
  }
}

export async function openWebSocket({
  origin,
  path,
  token,
  protocol = 'pin-glide.v1',
  target = TARGET,
  timeoutMs = 5_000,
} = {}) {
  assertWebSocketOrigin(origin, target);
  const route = serverRelativePath(path, 'WebSocket');
  const url = new URL(route, origin);
  if (url.origin !== origin) throw new Error('Smoke WebSocket path must stay on the adopted origin');
  const key = randomBytes(16).toString('base64');
  const transport = url.protocol === 'wss:' ? https : http;
  const headers = {
    Connection: 'Upgrade',
    Upgrade: 'websocket',
    'Sec-WebSocket-Version': '13',
    'Sec-WebSocket-Key': key,
    'Sec-WebSocket-Protocol': protocol,
  };
  if (token) headers.Authorization = `Bearer ${token}`;

  return new Promise((resolve, reject) => {
    const request = transport.request({
      protocol: url.protocol === 'wss:' ? 'https:' : 'http:',
      hostname: url.hostname,
      port: url.port || undefined,
      path: url.pathname,
      method: 'GET',
      headers,
      timeout: timeoutMs,
    });
    let settled = false;
    const fail = error => {
      if (settled) return;
      settled = true;
      reject(error);
    };
    request.on('timeout', () => {
      request.destroy(new Error('timed out opening the WebSocket'));
    });
    request.on('error', fail);
    request.on('response', response => {
      const chunks = [];
      response.on('data', chunk => chunks.push(chunk));
      response.on('end', () => {
        const text = Buffer.concat(chunks).toString('utf8');
        let body;
        try {
          body = text ? JSON.parse(text) : undefined;
        } catch {
          body = undefined;
        }
        if (settled) return;
        if (response.statusCode >= 400) {
          fail(new SmokeProblem(response.statusCode, body, response.headers['x-request-id']));
        } else {
          fail(new Error(`WebSocket upgrade failed with HTTP ${response.statusCode}`));
        }
      });
    });
    request.on('upgrade', (response, socket, head) => {
      if (settled) return socket.destroy();
      const expected = createHash('sha1').update(`${key}${websocketGUID}`).digest('base64');
      if (response.statusCode !== 101 || response.headers['sec-websocket-accept'] !== expected) {
        socket.destroy();
        return fail(new Error('invalid WebSocket upgrade response'));
      }
      if (response.headers['sec-websocket-protocol'] !== protocol) {
        socket.destroy();
        return fail(new Error('WebSocket upgrade did not select pin-glide.v1'));
      }
      settled = true;
      resolve(new WebSocketPeer(socket, head));
    });
    request.end();
  });
}
