import assert from 'node:assert/strict';

import { SmokeProblem } from './http.mjs';
import { associations } from './session_pairing_helpers.mjs';
import {
  associationRow,
  expirePairingSession,
  expectNoFrame,
  sessionPath,
  waitForPresence,
} from './session_pairing_support.mjs';

export async function expireRestrictedCredential(session, context) {
  const { pairing, delivery } = context.enrollment;
  const before = await associationRow(session, pairing.pairingID);
  if (before?.state !== 'active') throw new Error('Enrollment did not leave an active Pairing');
  await waitForPresence(session, 'pair1', pairing.pairingID, 'online');

  await expirePairingSession(session, pairing.sessionID);
  await assert.rejects(
    () => session.openSocket({ path: sessionPath(pairing), token: pairing.token }),
    error => error instanceof SmokeProblem
      && error.status === 401
      && error.code === 'credential_expired',
  );

  const after = await associationRow(session, pairing.pairingID);
  if (after?.state !== 'active') throw new Error('Expired B changed the activated Pairing');
  const listed = await associations(session, 'pair1');
  const current = listed.find(item => item.pairingId === pairing.pairingID);
  if (current?.presence !== 'online') {
    throw new Error('Expired B changed the shared Receiver delivery connection');
  }
  await expectNoFrame(delivery, 'Expired B disturbed the shared Receiver connection');

  return {
    session: { sessionId: pairing.sessionID, expired: true },
    pairing: { pairingId: pairing.pairingID, state: after.state },
    connection: { presence: current.presence, open: true },
  };
}
