import { randomUUID } from 'node:crypto';

import { renderBootstrapSQL, renderCleanupSQL } from './fixtures.mjs';
import { SmokeProblem } from './http.mjs';
import { openWebSocket } from './websocket.mjs';

function credential(sender, scope) {
  const found = sender.credentials.find(item => item.scope === scope);
  if (!found) throw new Error(`Sender ${sender.id} has no ${scope} credential`);
  return found;
}

export class SmokeSession {
  constructor({ target, remote, http, evidence, plan }) {
    this.target = target;
    this.remote = remote;
    this.http = http;
    this.evidence = evidence;
    this.plan = plan;
    this.receivers = new Set();
    this.requestIds = [];
    this.subjects = new Set();
  }

  static async create({ target, remote, http, evidence, plan }) {
    evidence.addSecret(plan.senders.flatMap(sender => sender.credentials.map(item => item.token)));
    await remote.sql(renderBootstrapSQL(plan));
    const session = new SmokeSession({ target, remote, http, evidence, plan });
    try {
      await session.verifyRows();
      await session.verifyAuthority();
      return session;
    } catch (error) {
      try {
        await session.cleanup();
      } catch (cleanupError) {
        throw new AggregateError([error, cleanupError], 'fixture setup and cleanup both failed');
      }
      throw error;
    }
  }

  sender(label) {
    const sender = this.plan.senders.find(item => item.id.endsWith(`-${label}`));
    if (!sender) throw new Error(`Unknown smoke Sender ${label}`);
    return sender;
  }

  token(label, scope) {
    return credential(this.sender(label), scope).token;
  }

  trackReceiver(receiverID) {
    if (receiverID) this.receivers.add(receiverID);
    if (receiverID) this.subjects.add(receiverID);
    return receiverID;
  }

  newOperationID(label) {
    return `smoke-${this.plan.runId}-${label}-${randomUUID()}`;
  }

  async request(options) {
    try {
      const result = await this.http.call(options);
      if (result.requestId) this.requestIds.push(result.requestId);
      return result;
    } catch (error) {
      if (error.requestId) this.requestIds.push(error.requestId);
      throw error;
    }
  }

  async createPairing(label, mode = 'new_receiver', receiverID = '') {
    const body = { mode };
    if (receiverID) body.receiverId = receiverID;
    const created = await this.request({
      method: 'POST',
      path: '/v1/pairing-sessions',
      body,
    });
    if (created.status !== 201) throw new Error(`Pairing Session creation returned ${created.status}`);
    const sessionID = created.body.session.sessionId;
    if (sessionID) this.subjects.add(sessionID);
    this.trackReceiver(created.body.session.receiverId);
    if (created.body.pairingToken) this.evidence.addSecret(created.body.pairingToken);
    const authorized = await this.request({
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(sessionID)}/authorize`,
      token: this.token(label, 'user_session'),
      body: {},
    });
    if (authorized.body.state !== 'awaiting_confirmation') {
      throw new Error(`Pairing Session state is ${authorized.body.state}, not awaiting_confirmation`);
    }
    return {
      sessionID,
      receiverID: authorized.body.receiverId,
      pairingID: authorized.body.pairingId,
      token: created.body.pairingToken,
    };
  }

  async completePairing(label, pairing, socket) {
    if (!socket) socket = await this.openSocket({
      path: `/v1/pairing-sessions/${encodeURIComponent(pairing.sessionID)}/events`,
      token: pairing.token,
    });
    let senderSocket;
    try {
      senderSocket = await this.openSocket({
        path: `/v1/pairing-sessions/${encodeURIComponent(pairing.sessionID)}/events`,
        token: this.token(label, 'user_session'),
      });
      await socket.next();
      await senderSocket.next();
      socket.sendJSON({
        version: 1,
        type: 'pairing_relay',
        sessionId: pairing.sessionID,
        payload: Buffer.from(`smoke-${pairing.sessionID}`).toString('base64'),
      });
      const relayed = await senderSocket.next();
      if (relayed.json?.type !== 'pairing_relay' || relayed.json?.side !== 'receiver') {
        throw new Error('Pairing relay did not reach the management participant');
      }
      await this.completePairingSide(label, pairing);
      await this.completePairingSide(label, pairing, pairing.token);
    } finally {
      await Promise.allSettled([
        senderSocket?.close(),
        socket.close(),
      ]);
    }
    if (pairing.pairingID) this.trackReceiver(pairing.receiverID);
  }

  async completePairingSide(label, pairing, restricted) {
    const path = `/v1/pairing-sessions/${encodeURIComponent(pairing.sessionID)}/complete`;
    const result = await this.request({
      method: 'POST',
      path,
      token: restricted || this.token(label, 'user_session'),
      body: { keyConfirmation: 'confirmed' },
    });
    return result.body;
  }

  async redeemReceiver(pairing) {
    const result = await this.request({
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(pairing.sessionID)}/receiver-token`,
      token: pairing.token,
      body: {
        operationId: this.newOperationID('redeem'),
        attempt: 0,
      },
    });
    return result.body;
  }

  openSocket({ path, token }) {
    return openWebSocket({ origin: this.target.wsOrigin, path, token, target: this.target });
  }

  async verifyRows({ minimumActive = false } = {}) {
    const senderIDs = this.plan.senders.map(sender => sender.id);
    const rows = await this.remote.sqlJSON(`
SELECT json_build_object(
  'accounts', (
    SELECT count(*)
    FROM pin_glide.identity_accounts
    WHERE id IN (${this.plan.accounts.map(account => `'${account.id}'`).join(',')})
  ),
  'senders', (
    SELECT count(*)
    FROM pin_glide.identity_sender_registrations
    WHERE id IN (${senderIDs.map(id => `'${id}'`).join(',')})
      AND state = 'active'
  ),
  'active', (
    SELECT count(*)
    FROM pin_glide.identity_credentials
    WHERE sender_id IN (${senderIDs.map(id => `'${id}'`).join(',')})
      AND revoked_at IS NULL
      AND expires_at > now()
  ),
  'badDigests', (
    SELECT count(*)
    FROM pin_glide.identity_credentials
    WHERE sender_id IN (${senderIDs.map(id => `'${id}'`).join(',')})
      AND token_digest !~ '^[0-9a-f]{64}$'
  ),
  'badLifetimes', (
    SELECT count(*)
    FROM pin_glide.identity_credentials
    WHERE sender_id IN (${senderIDs.map(id => `'${id}'`).join(',')})
      AND (
        (scope IN ('user_session', 'uploader') AND expires_at - issued_at <> interval '3 hours')
        OR (scope = 'refresh' AND expires_at - issued_at <> interval '7 days')
      )
  ),
  'families', (
    SELECT count(DISTINCT family_id)
    FROM pin_glide.identity_credentials
    WHERE sender_id IN (${senderIDs.map(id => `'${id}'`).join(',')})
  )
);
`);
    if (
      rows.accounts !== this.plan.accounts.length
      || rows.senders !== this.plan.senders.length
      || (minimumActive ? rows.active < this.plan.senders.length * 3 : rows.active !== this.plan.senders.length * 3)
      || rows.badDigests !== 0
      || rows.badLifetimes !== 0
      || rows.families !== this.plan.senders.length
    ) {
      throw new Error(`Fixture rows are not normal authority: ${JSON.stringify(rows)}`);
    }
  }

  async verifyAuthority() {
    const missingMessage = `smoke-missing-${this.plan.runId}`;
    for (const sender of this.plan.senders) {
      const management = credential(sender, 'user_session').token;
      const uploader = credential(sender, 'uploader').token;
      const renewal = credential(sender, 'refresh').token;
      const listed = await this.request({ path: '/v1/self/receivers', token: management });
      if (listed.status !== 200) throw new Error('Management credential did not authorize its Receiver list');
      try {
        await this.request({ path: `/v1/messages/${missingMessage}`, token: uploader });
        throw new Error('Upload credential unexpectedly found a missing Message');
      } catch (error) {
        if (!(error instanceof SmokeProblem) || error.status !== 404 || error.code !== 'resource_not_found') {
          throw new Error(`Upload credential was not accepted: ${error.message}`);
        }
      }
      const rotated = await this.request({
        method: 'POST',
        path: '/v1/auth/refresh',
        body: {
          refreshToken: renewal,
          operationId: `smoke-${this.plan.runId}-refresh-${sender.id}`,
          attempt: 0,
        },
      });
      if (rotated.body?.senderId !== sender.id) {
        throw new Error(`Refresh rotated the wrong Sender: ${rotated.body?.senderId || 'missing'}`);
      }
      sender.credentials = [
        { scope: 'user_session', ...rotated.body.userSession },
        { scope: 'uploader', ...rotated.body.uploader },
        { scope: 'refresh', ...rotated.body.refresh },
      ];
      this.evidence.addSecret(sender.credentials.map(item => item.token));
      const rotatedManagement = credential(sender, 'user_session').token;
      const rotatedUploader = credential(sender, 'uploader').token;
      const afterRefresh = await this.request({ path: '/v1/self/receivers', token: rotatedManagement });
      if (afterRefresh.status !== 200) {
        throw new Error('Rotated management credential did not authorize its Receiver list');
      }
      try {
        await this.request({ path: `/v1/messages/${missingMessage}`, token: rotatedUploader });
        throw new Error('Rotated upload credential unexpectedly found a missing Message');
      } catch (error) {
        if (!(error instanceof SmokeProblem) || error.status !== 404 || error.code !== 'resource_not_found') {
          throw new Error(`Rotated upload credential was not accepted: ${error.message}`);
        }
      }
    }
    await this.verifyRows({ minimumActive: true });
  }

  async cleanup() {
    await this.remote.sql(renderCleanupSQL(this.plan, [...this.receivers], this.requestIds, [...this.subjects]));
    this.receivers.clear();
    this.subjects.clear();
    this.requestIds.length = 0;
  }

  async close() {
    await this.cleanup();
  }
}
