import { createHash, randomUUID } from 'node:crypto';

import {
  credentialLifetimeInterval,
  fixtureSender,
  renderBootstrapSQL,
  renderCleanupSQL,
  sqlTextArray,
} from './fixtures.mjs';
import { withFailureJoiningCleanup } from './cleanup.mjs';
import { SmokeProblem } from './http.mjs';
import { renderRetentionCleanupSQL } from './retention_checks.mjs';
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
    this.startedAt = new Date().toISOString();
    this.receivers = new Set();
    this.requestIds = [];
    this.subjects = new Set();
    this.secrets = new Set(evidence.secretValues?.() || []);
    this.errorBodies = [];
    this.retentionRunIDs = new Set();
  }

  static async create({ target, remote, http, evidence, plan }) {
    await remote.sql(renderBootstrapSQL(plan));
    const session = new SmokeSession({ target, remote, http, evidence, plan });
    session.addSecret(plan.senders.flatMap(sender => [
      ...sender.credentials.map(item => item.token),
      ...(sender.replacementCredentials || []).map(item => item.token),
    ]));
    try {
      const rows = await session.verifyRows();
      const ownership = await session.verifyAuthority();
      evidence.record({
        scenario: 'guarded SQL fixture bootstrap and Account/Sender ownership',
        status: 'passed',
        details: {
          accounts: rows.accounts,
          senders: rows.senders,
          activeCredentials: rows.active,
          ownership,
          fixtureIdentity: 'synthetic-sql-not-provider-verified',
          requestIds: [...session.requestIds],
        },
      });
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
    return fixtureSender(this.plan, label);
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

  addSecret(value) {
    for (const item of Array.isArray(value) ? value : [value]) {
      if (!item) continue;
      this.secrets.add(item);
      this.evidence.addSecret(item);
    }
  }

  secretValues() {
    return [...this.secrets];
  }

  async request(options) {
    return this.send(options, false);
  }

  async droppedRequest(options) {
    return this.send(options, true);
  }

  abandonedRequest(options) {
    const { request, abort, aborted } = this.http.callAbandoned(options);
    return {
      request: request.then(result => {
        this.trackRequestId(result);
        return result;
      }, error => {
        this.trackRequestId(error);
        // The caller's own abort is what proves cancellation; a timeout or
        // service failure is a different outcome.
        error.aborted = aborted();
        throw error;
      }),
      abort,
      aborted,
    };
  }

  trackRequestId(value) {
    if (value?.requestId) this.requestIds.push(value.requestId);
  }

  async send(options, dropped) {
    try {
      const result = dropped
        ? await this.http.callDropped(options)
        : await this.http.call(options);
      this.trackRequestId(result);
      return result;
    } catch (error) {
      this.trackRequestId(error);
      if (error instanceof SmokeProblem && error.body) this.errorBodies.push(error.body);
      throw error;
    }
  }

  async expectProblem(options, status, code) {
    try {
      await this.request(options);
    } catch (error) {
      if (!(error instanceof SmokeProblem)) throw error;
      if (error.status !== status || error.code !== code) {
        throw new Error(
          `Expected HTTP ${status} ${code}, received ${error.status} ${error.code || '(no code)'}`,
        );
      }
      return { status: error.status, code: error.code, requestId: error.requestId };
    }
    throw new Error(`Expected HTTP ${status} ${code}, but the request succeeded`);
  }

  async verifyManagement(token) {
    const result = await this.request({ path: '/v1/self/receivers', token });
    if (result.status !== 200) throw new Error(`Management credential returned ${result.status}`);
    return result;
  }

  async verifyUploader(token) {
    return this.expectProblem(
      { path: `/v1/messages/smoke-missing-${this.plan.runId}`, token },
      404,
      'resource_not_found',
    );
  }

  async refreshAttempt(label, { operationID, attempt = 0, token } = {}) {
    const sender = this.sender(label);
    return this.request({
      method: 'POST',
      path: '/v1/auth/refresh',
      body: {
        refreshToken: token || credential(sender, 'refresh').token,
        operationId: operationID,
        attempt,
      },
    });
  }

  acceptRefresh(label, body, { operationID, attempt }) {
    const sender = this.sender(label);
    if (body?.accountId !== sender.accountID || body?.senderId !== sender.id) {
      throw new Error(`Refresh returned the wrong Sender ownership for ${label}`);
    }
    if (body.operationId !== operationID || body.attempt !== attempt) {
      throw new Error(
        `Refresh returned ${body.operationId}/${body.attempt}, expected ${operationID}/${attempt}`,
      );
    }
    const credentials = [
      { ...body.userSession, scope: 'user_session' },
      { ...body.uploader, scope: 'uploader' },
      { ...body.refresh, scope: 'refresh' },
    ];
    for (const item of credentials) {
      if (!item.token || !Number.isFinite(Date.parse(item.expiresAt))) {
        throw new Error(`Refresh returned an incomplete ${item.scope} credential`);
      }
      item.digest = createHash('sha256').update(item.token).digest('hex');
    }
    if (new Set(credentials.map(item => item.token)).size !== credentials.length) {
      throw new Error('Refresh returned non-independent credential tokens');
    }
    sender.credentials = credentials;
    sender.latestRefresh = { operationID, attempt };
    sender.pendingRefresh = undefined;
    this.addSecret(credentials.map(item => item.token));
    return body;
  }

  async refresh(label, options = {}) {
    const operationID = options.operationID || this.newOperationID(`refresh-${label}`);
    const attempt = options.attempt ?? 0;
    const request = {
      method: 'POST',
      path: '/v1/auth/refresh',
      body: {
        refreshToken: options.token || this.token(label, 'refresh'),
        operationId: operationID,
        attempt,
      },
    };
    if (options.dropped) {
      const result = await this.droppedRequest(request);
      this.sender(label).pendingRefresh = { operationID, attempt };
      return result;
    }
    const result = await this.request(request);
    if (result.cacheControl !== 'no-store') {
      throw new Error('Refresh success was cacheable');
    }
    this.acceptRefresh(label, result.body, { operationID, attempt });
    return result.body;
  }

  async createPairingSession(options = {}) {
    const mode = options.mode || 'new_receiver';
    const receiverID = options.receiverID || '';
    const token = options.token || '';
    const body = { mode };
    if (receiverID) body.receiverId = receiverID;
    const created = await this.request({
      method: 'POST',
      path: '/v1/pairing-sessions',
      token,
      body,
    });
    if (created.status !== 201) throw new Error(`Pairing Session creation returned ${created.status}`);
    const view = created.body?.session;
    const sessionID = view?.sessionId;
    if (!sessionID) throw new Error('Pairing Session creation returned no session ID');
    if (view.mode !== mode) throw new Error(`Pairing Session mode is ${view.mode}, expected ${mode}`);
    if (mode !== 'new_receiver' && view.receiverId !== receiverID) {
      throw new Error('Pairing Session did not preserve its fixed Receiver target');
    }
    const tokenExpiresAt = created.body.pairingTokenExpiresAt;
    if (!created.body.pairingToken || Date.parse(tokenExpiresAt) !== Date.parse(view.expiresAt)) {
      throw new Error('Pairing Session did not return an equally bounded restricted credential');
    }
    this.subjects.add(sessionID);
    this.trackReceiver(view.receiverId);
    this.addSecret(created.body.pairingToken);
    return {
      sessionID,
      receiverID: view.receiverId,
      mode: view.mode,
      state: view.state,
      expiresAt: view.expiresAt,
      tokenExpiresAt,
      token: created.body.pairingToken,
    };
  }

  async claimPairing(label, pairing, options = {}) {
    const body = {};
    if (options.alias !== undefined) body.alias = options.alias;
    const authorized = await this.request({
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(pairing.sessionID)}/authorize`,
      token: this.token(label, 'user_session'),
      body,
    });
    if (authorized.body.state !== 'awaiting_confirmation') {
      throw new Error(`Pairing Session state is ${authorized.body.state}, not awaiting_confirmation`);
    }
    if (authorized.body.receiverId !== pairing.receiverID || authorized.body.mode !== pairing.mode) {
      throw new Error('Pairing Session claim changed its fixed mode or Receiver target');
    }
    return {
      ...pairing,
      receiverID: authorized.body.receiverId,
      pairingID: authorized.body.pairingId,
      state: authorized.body.state,
    };
  }

  async createPairing(label, options = {}) {
    if (typeof options === 'string') options = { mode: options };
    const created = await this.createPairingSession(options);
    return this.claimPairing(label, created, { alias: options.alias });
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

  async redeemReceiver(pairing, options = {}) {
    const request = {
      method: 'POST',
      path: `/v1/pairing-sessions/${encodeURIComponent(pairing.sessionID)}/receiver-token`,
      token: pairing.token,
      body: {
        operationId: options.operationID || this.newOperationID('redeem'),
        attempt: options.attempt ?? 0,
      },
    };
    const result = options.dropped
      ? await this.droppedRequest(request)
      : await this.request(request);
    if (options.dropped) return result;
    if (result.cacheControl !== 'no-store') throw new Error('Receiver credential issuance was cacheable');
    this.addSecret(result.body?.receiverToken);
    return result.body;
  }

  openSocket(options) {
    return openWebSocket({ origin: this.target.wsOrigin, target: this.target, ...options });
  }

  async verifyRows() {
    const senderIDs = this.plan.senders.map(sender => sender.id);
    const accountIDs = this.plan.accounts.map(account => account.id);
    const rows = await this.remote.sqlJSON(`
SELECT json_build_object(
  'accounts', (
    SELECT count(*)
    FROM pin_glide.identity_accounts
    WHERE id = ANY(${sqlTextArray(accountIDs)})
  ),
  'senders', (
    SELECT count(*)
    FROM pin_glide.identity_sender_registrations
    WHERE id = ANY(${sqlTextArray(senderIDs)})
      AND state = 'active'
  ),
  'active', (
    SELECT count(*)
    FROM pin_glide.identity_credentials
    WHERE sender_id = ANY(${sqlTextArray(senderIDs)})
      AND revoked_at IS NULL
      AND expires_at > now()
  ),
  'badDigests', (
    SELECT count(*)
    FROM pin_glide.identity_credentials
    WHERE sender_id = ANY(${sqlTextArray(senderIDs)})
      AND token_digest !~ '^[0-9a-f]{64}$'
  ),
  'badLifetimes', (
    SELECT count(*)
    FROM pin_glide.identity_credentials
    WHERE sender_id = ANY(${sqlTextArray(senderIDs)})
      AND (
        (
          scope IN ('user_session', 'uploader')
          AND expires_at - issued_at <> interval '${credentialLifetimeInterval('user_session')}'
        )
        OR (
          scope = 'refresh'
          AND expires_at - issued_at <> interval '${credentialLifetimeInterval('refresh')}'
        )
      )
  ),
  'families', (
    SELECT count(DISTINCT family_id)
    FROM pin_glide.identity_credentials
    WHERE sender_id = ANY(${sqlTextArray(senderIDs)})
  )
);
`);
    if (
      rows.accounts !== this.plan.accounts.length
      || rows.senders !== this.plan.senders.length
      || rows.active !== this.plan.senders.length * 3
      || rows.badDigests !== 0
      || rows.badLifetimes !== 0
      || rows.families !== this.plan.senders.length
    ) {
      throw new Error(`Fixture rows are not normal authority: ${JSON.stringify(rows)}`);
    }
    return rows;
  }

  async verifyAuthority() {
    const verified = [];
    for (const sender of this.plan.senders) {
      const management = credential(sender, 'user_session').token;
      const uploader = credential(sender, 'uploader').token;
      const renewal = credential(sender, 'refresh').token;
      await this.verifyManagement(management);
      await this.verifyUploader(uploader);
      await this.expectProblem(
        { path: '/v1/self/receivers', token: renewal },
        401,
        'credential_invalid',
      );
      const operationID = this.newOperationID(`authority-${sender.label}`);
      const rotated = await this.refresh(sender.label, { operationID, attempt: 0 });
      await this.verifyManagement(rotated.userSession.token);
      await this.verifyUploader(rotated.uploader.token);
      verified.push({ senderId: sender.id, accountId: rotated.accountId, operationId: operationID, attempt: 0 });
    }
    return verified;
  }

  async cleanup() {
    await withFailureJoiningCleanup(
      () => this.remote.sql(renderRetentionCleanupSQL(this.plan, [...this.retentionRunIDs])),
      () => this.remote.sql(renderCleanupSQL(
        this.plan,
        [...this.receivers],
        this.requestIds,
        [...this.subjects],
      )),
      'Retention fixtures and the ordinary smoke fixture cleanup both failed',
    );
    this.receivers.clear();
    this.subjects.clear();
    this.requestIds.length = 0;
    this.errorBodies.length = 0;
    this.retentionRunIDs.clear();
  }

  async close() {
    await this.cleanup();
  }
}
