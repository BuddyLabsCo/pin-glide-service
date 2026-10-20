import { escapeSQLString } from './fixtures.mjs';

export async function receiverState(session, receiverID) {
  const rows = await session.remote.sqlJSON(`
SELECT json_build_object(
  'state', state
)
FROM pin_glide.identity_receiver_registrations
WHERE id = ${escapeSQLString(receiverID)};
`);
  return rows.state;
}

export async function associations(session, label) {
  const listed = await session.verifyManagement(session.token(label, 'user_session'));
  return listed.body.items || [];
}

export async function pairReceiver(session, label) {
  const pairing = await session.createPairing(label);
  if (!pairing.pairingID || !pairing.receiverID) throw new Error('Pairing activation returned no association');
  await session.completePairing(label, pairing);
  const issued = await session.redeemReceiver(pairing);
  if (!issued.receiverToken || issued.receiverId !== pairing.receiverID) {
    throw new Error('Receiver credential does not belong to the paired Receiver');
  }
  const listed = await associations(session, label);
  if (!listed.some(item => item.pairingId === pairing.pairingID)) {
    throw new Error('Activated Pairing is absent from the Sender association list');
  }
  return { ...pairing, receiverToken: issued.receiverToken };
}

export async function addSenderToReceiver(session, label, pairing) {
  const added = await session.createPairing(label, {
    mode: 'add_sender',
    receiverID: pairing.receiverID,
    token: pairing.receiverToken,
  });
  if (!added.pairingID || added.receiverID !== pairing.receiverID) {
    throw new Error('add_sender did not target the paired Receiver');
  }
  await session.completePairing(label, added);
  const listed = await associations(session, label);
  if (!listed.some(item => item.pairingId === added.pairingID)) {
    throw new Error('add_sender Pairing is absent from the Sender association list');
  }
  return added;
}
