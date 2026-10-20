import { withFailureJoiningCleanup } from './cleanup.mjs';
import { withDeliveryBatchLock, withDeliveryStorageFault } from './delivery_faults.mjs';
import {
  scenarioDuplicates,
  scenarioMixed,
  scenarioOutcomes,
  scenarioRetainedLookup,
} from './session_delivery_lifecycle.mjs';
import { scenarioAuthority } from './session_delivery_authority.mjs';
import {
  deliveryItem,
  deliveryDurationQuery,
  expectMetadata,
  expectNoMessage,
  expectResult,
  liveTarget,
  messageID,
  metadata,
  nextDelivery,
  openReceiver,
  runDeliveryScenario,
  sendReceipt,
  upload,
  waitForAdmitted,
  waitForMetadata,
} from './session_delivery_support.mjs';

const deliveryWSS = ['delivery', 'receipt'];

async function scenarioSendFailed(session, context) {
  const target = liveTarget(session, context);
  const socket = await openReceiver(session, target.receiverToken);
  // The aggregate payload must exceed the Receiver peer's socket buffers.
  // Keeping the connection open and unread makes writes that cannot make room
  // exceed the service's write bound, while writes that fit still await a
  // Receipt, so the scenario observes both outcomes without a disconnect race.
  const messages = Array.from({ length: 16 }, (_, index) => messageID(session, `send-failed-${index}`));
  let pending;
  try {
    socket.pauseReads();
    await new Promise(resolve => setTimeout(resolve, 100));
    const started = Date.now();
    pending = messages.map((message, index) => upload(
      session,
      'pair2',
      message,
      [deliveryItem(target, {
        nonce: Buffer.alloc(12, index).toString('base64'),
        ciphertext: Buffer.alloc(700 * 1024, 7).toString('base64'),
      })],
    ).then(response => ({ response, message, elapsedMs: Date.now() - started })));
    // One claimed row is enough to prove storage admission; the rest may
    // still be claiming when the first write's deadline starts.
    await waitForAdmitted(session, 'pair2', messages, 1);
    const admittedOffsetMs = Date.now() - started;
    const responses = await Promise.all(pending);
    const failed = responses.filter(({ response }) => (
      response.body?.results?.[0]?.status === 'send_failed'
    ));
    if (failed.length === 0) {
      throw new Error(`Paused Receiver produced no send_failed result: ${JSON.stringify(
        responses.map(item => item.response.body?.results?.[0]?.status),
      )}`);
    }
    const observedStatuses = responses.map(item => item.response.body?.results?.[0]?.status);
    // Entries still waiting for the Receiver when the batch budget ends are
    // honestly interrupted; the scenario only needs at least one bounded
    // write failure among the blocked entries.
    const unexpected = observedStatuses.filter(status => (
      status !== 'send_failed' && status !== 'ack_timeout' && status !== 'interrupted'
    ));
    if (unexpected.length > 0) {
      throw new Error(`Paused Receiver produced unexpected outcomes: ${JSON.stringify(observedStatuses)}`);
    }
    for (const { elapsedMs: requestElapsedMs } of responses) {
      if (requestElapsedMs > 15_500) {
        throw new Error(`Delivery request took ${requestElapsedMs}ms, beyond its 15-second budget`);
      }
    }
    // The stored send_failed duration is the write attempt itself, without
    // upload or queueing time, so the longest failed write proves the 2s bound.
    const failureDurations = await session.remote.sqlJSON(
      deliveryDurationQuery(session, 'pair2', failed.map(item => item.message), 'send_failed'),
    );
    // The first blocked write exhausts the deadline; later entries queued on
    // the same closed socket may fail sooner, so one failure must land at the
    // bound and none may exceed it.
    if (
      Number(failureDurations.rows) !== failed.length
      || Number(failureDurations.maximumMs) < 1_500
      || Number(failureDurations.maximumMs) > 2_500
    ) {
      throw new Error(`Failed Delivery writes reached ${failureDurations.maximumMs}ms, outside the two-second bound`);
    }
    for (const { response, message } of failed) {
      expectResult(response, 0, target.receiverID, 'send_failed', 'send_failed');
      expectMetadata(await metadata(session, 'pair2', message), [{
        receiverID: target.receiverID,
        status: 'send_failed',
        code: 'send_failed',
      }]);
    }
    return {
      writeFailure: failed[0].response.body.results[0].status,
      observedStatuses: [...new Set(observedStatuses)],
      failedEntries: failed.length,
      failedElapsedMs: failed.map(item => item.elapsedMs),
      failureDurations,
      requestElapsedMs: responses.map(item => item.elapsedMs),
      concurrentRequests: messages.length,
      admittedOffsetMs,
      writeBounded: true,
      batchBounded: responses.every(item => item.elapsedMs <= 15_500),
      arrivalNotGuaranteedAbsent: true,
    };
  } finally {
    if (pending) await Promise.allSettled(pending);
    socket.resumeReads();
    await socket.close();
  }
}

async function scenarioBatchBudget(session, context) {
  const live = liveTarget(session, context);
  const message = messageID(session, 'batch-budget');
  let started;
  const outcome = await withDeliveryBatchLock(session.remote, async () => {
    started = Date.now();
    try {
      // The client waits past the service's own budget so it observes the
      // service's 503 rather than an HTTP peer timeout.
      await session.request({
        method: 'POST',
        path: '/v1/messages',
        token: session.token('pair2', 'uploader'),
        body: { messageId: message, items: [deliveryItem(live)] },
        timeoutMs: 20_000,
      });
      return { status: 207 };
    } catch (error) {
      if (error?.status !== 503 || error?.code !== 'delivery_status_unavailable') throw error;
      return { status: error.status, code: error.code };
    }
  }, { seconds: 20 });
  const elapsedMs = Date.now() - started;
  if (elapsedMs < 14_500 || elapsedMs > 16_500) {
    throw new Error(`A blocked Delivery batch answered in ${elapsedMs}ms, outside the fifteen-second budget`);
  }
  return { ...outcome, elapsedMs, batchBounded: true };
}

async function scenarioUncertainty(session, context) {
  const live = liveTarget(session, context);
  const socket = await session.openSocket({ path: '/v1/receiver/events', token: live.receiverToken });
  const disconnectedMessage = messageID(session, 'disconnected');
  let abandoned;
  try {
    abandoned = session.abandonedRequest({
      method: 'POST',
      path: '/v1/messages',
      token: session.token('pair2', 'uploader'),
      body: { messageId: disconnectedMessage, items: [deliveryItem(live)] },
    });
    const frame = await nextDelivery(socket, live);
    abandoned.abort();
    let aborted = false;
    try {
      await abandoned.request;
    } catch (error) {
      aborted = error.aborted === true;
    }
    if (!aborted || !abandoned.aborted()) {
      throw new Error('Caller received the abandoned response instead of disconnecting');
    }
    sendReceipt(socket, frame);
    const continued = await waitForMetadata(session, 'pair2', disconnectedMessage, [{
      receiverID: live.receiverID,
      status: 'received',
    }]);
    await expectNoMessage(socket, 'Disconnected caller caused a compensating delivery write');
    return {
      callerDisconnected: aborted,
      continued: continued.body.deliveries[0].status,
    };
  } finally {
    if (abandoned) await abandoned.request.catch(() => {});
    await socket.close();
  }
}

async function scenarioRestart(session, context) {
  const live = liveTarget(session, context);
  const socket = await session.openSocket({ path: '/v1/receiver/events', token: live.receiverToken });
  const abandonedMessage = messageID(session, 'restart-abandoned');
  const interruptedMessage = messageID(session, 'restart-interrupted');
  let abandoned;
  let newer;
  let duplicate;
  try {
    abandoned = session.abandonedRequest({
      method: 'POST',
      path: '/v1/messages',
      token: session.token('pair2', 'uploader'),
      body: { messageId: abandonedMessage, items: [deliveryItem(live)] },
    });
    await nextDelivery(socket, live);
    abandoned.abort();
    let callerDisconnected = false;
    try {
      await abandoned.request;
    } catch (error) {
      callerDisconnected = error.aborted === true;
    }
    if (!callerDisconnected || !abandoned.aborted()) {
      throw new Error('Restart scenario did not disconnect the caller');
    }

    await session.remote.restartAPI();
    const continued = await waitForMetadata(session, 'pair2', abandonedMessage, [{
      receiverID: live.receiverID,
      status: 'interrupted',
    }], 15_000);
    const reconnected = await openReceiver(session, live.receiverToken);
    try {
      newer = upload(session, 'pair2', interruptedMessage, [deliveryItem(live)]);
      const frame = await nextDelivery(reconnected, live);
      sendReceipt(reconnected, frame);
      expectResult(await newer, 0, live.receiverID, 'received');
      duplicate = await upload(session, 'pair2', abandonedMessage, [deliveryItem(live)]);
      expectResult(duplicate, 0, live.receiverID, 'interrupted', 'interrupted');
      await expectNoMessage(reconnected, 'Restart replayed or resent an interrupted Delivery');
      return {
        callerDisconnected,
        processRestart: true,
        interrupted: continued.body.deliveries[0].status,
        resumedSend: false,
        reconnected: true,
        newerTraffic: 'received',
      };
    } finally {
      await Promise.allSettled([newer, duplicate].filter(Boolean));
      await reconnected.close();
    }
  } finally {
    if (abandoned) await abandoned.request.catch(() => {});
    await socket.close();
  }
}

async function scenarioPostgresOutage(session, context) {
  const live = liveTarget(session, context);
  const socket = await session.openSocket({ path: '/v1/receiver/events', token: live.receiverToken });
  const message = messageID(session, 'postgres-outage');
  let stopped = false;
  try {
    return await withFailureJoiningCleanup(
      async () => {
        await session.remote.stopPostgres();
        stopped = true;
        const started = Date.now();
        let uncertain;
        try {
          const result = await session.request({
            method: 'POST',
            path: '/v1/messages',
            token: session.token('pair2', 'uploader'),
            body: { messageId: message, items: [deliveryItem(live)] },
          });
          if (result.status !== 503 || result.body?.code !== 'service_unavailable') {
            throw new Error(`Upload with PostgreSQL down returned ${result.status}: ${JSON.stringify(result.body)}`);
          }
          uncertain = result;
        } catch (error) {
          if (error?.status !== 503 || error?.code !== 'service_unavailable') throw error;
          uncertain = error;
        }
        const elapsedMs = Date.now() - started;
        await session.remote.startPostgres();
        stopped = false;
        const deadline = Date.now() + 30_000;
        for (;;) {
          try {
            const health = await session.request({ path: '/health' });
            if (health.body?.status === 'ok') break;
          } catch {}
          if (Date.now() > deadline) throw new Error('API did not report healthy after PostgreSQL returned');
          await new Promise(resolve => setTimeout(resolve, 200));
        }
        await expectNoMessage(socket, 'PostgreSQL outage caused a compensating delivery write');
        return {
          httpStatus: uncertain.status,
          code: uncertain.code,
          elapsedMs,
          writerRestored: true,
          noCompensatingSend: true,
        };
      },
      async () => {
        if (stopped) await session.remote.startPostgres();
      },
      'PostgreSQL outage and restoration both failed',
    );
  } finally {
    await socket.close();
  }
}

async function scenarioStorageUncertainty(session, context) {
  const live = liveTarget(session, context);
  const socket = await openReceiver(session, live.receiverToken);
  const message = messageID(session, 'storage');
  try {
    const result = await withDeliveryStorageFault(
      session.remote,
      async () => {
        const uncertain = await session.expectProblem({
          method: 'POST',
          path: '/v1/messages',
          token: session.token('pair2', 'uploader'),
          body: { messageId: message, items: [deliveryItem(live)] },
        }, 503, 'delivery_status_unavailable');
        // Metadata is registered before any WSS side effect, so an entry whose
        // storage claim failed was never sent.
        await expectNoMessage(socket, 'Storage failure sent a Delivery before its metadata');
        return { storageUnavailable: uncertain, noSend: true };
      },
    );
    return result;
  } finally {
    await socket.close();
  }
}

async function scenarioFinalizationUncertainty(session, context) {
  const target = liveTarget(session, context);
  const socket = await openReceiver(session, target.receiverToken);
  const message = messageID(session, 'finalization');
  let response;
  try {
    response = upload(session, 'pair2', message, [deliveryItem(target)]);
    const frame = await nextDelivery(socket, target);
    const uncertain = await withDeliveryStorageFault(
      session.remote,
      async () => {
        sendReceipt(socket, frame);
        const result = await response.catch(error => {
          if (error?.status === 503 && error?.code === 'delivery_status_unavailable') return error;
          throw error;
        });
        if (result.status !== 503 || result.code !== 'delivery_status_unavailable') {
          throw new Error(`Finalization failure returned ${result.status}: ${result.code}`);
        }
        return result;
      },
    );
    const restored = expectMetadata(await metadata(session, 'pair2', message), [{
      receiverID: target.receiverID,
      status: 'interrupted',
      code: 'interrupted',
    }]);
    await expectNoMessage(socket, 'Uncertain finalization caused a compensating delivery write');
    return {
      finalizationUncertain: uncertain.code,
      afterRestore: restored[target.receiverID].status,
      noCompensatingSend: true,
    };
  } finally {
    if (response) await response.catch(() => {});
    await socket.close();
  }
}

export async function runDeliveryScenarios(session, context = {}) {
  if (!context.currentPairing || !context.shared || !context.enrollment) {
    throw new Error('Delivery smoke scenarios require the shared Pairing context');
  }
  const scenarios = [];
  scenarios.push(await runDeliveryScenario(
    session,
    'indexed Delivery outcomes distinguish envelope, item, authority and transport results',
    { http: ['PostMessages', 'GetMessage'], wss: deliveryWSS },
    () => scenarioMixed(session, context),
  ));
  scenarios.push(await runDeliveryScenario(
    session,
    'simultaneous duplicates share one send and retained terminal result',
    { http: ['PostMessages', 'GetMessage'], wss: deliveryWSS },
    () => scenarioDuplicates(session, context),
  ));
  scenarios.push(await runDeliveryScenario(
    session,
    'a bounded write failure reports send_failed without claiming absence',
    { http: ['PostMessages', 'GetMessage'], wss: ['delivery'] },
    () => scenarioSendFailed(session, context),
  ));
  scenarios.push(await runDeliveryScenario(
    session,
    'a blocked Delivery batch ends inside the fifteen-second budget',
    { http: ['PostMessages'], wss: [] },
    () => scenarioBatchBudget(session, context),
  ));
  scenarios.push(await runDeliveryScenario(
    session,
    'current-authority-before-dedup and retained history after removal',
    {
      http: ['PostMessages', 'GetMessage', 'DeleteSelfPairing', 'PostAuthLogout'],
      wss: deliveryWSS,
    },
    () => scenarioAuthority(session, context),
  ));
  scenarios.push(await runDeliveryScenario(
    session,
    'Receipt correlation, timeout and corrupt-frame outcomes',
    { http: ['PostMessages', 'GetMessage'], wss: deliveryWSS },
    () => scenarioOutcomes(session, context),
  ));
  scenarios.push(await runDeliveryScenario(
    session,
    'disconnected HTTP caller does not cancel admitted Delivery',
    { http: ['PostMessages', 'GetMessage'], wss: deliveryWSS },
    () => scenarioUncertainty(session, context),
  ));
  scenarios.push(await runDeliveryScenario(
    session,
    'missing Delivery storage reports uncertainty',
    { http: ['PostMessages'], wss: [] },
    () => scenarioStorageUncertainty(session, context),
  ));
  scenarios.push(await runDeliveryScenario(
    session,
    'unconfirmed finalization reports uncertainty and resolves interrupted without resend',
    { http: ['PostMessages', 'GetMessage'], wss: deliveryWSS },
    () => scenarioFinalizationUncertainty(session, context),
  ));
  scenarios.push(await runDeliveryScenario(
    session,
    'process restart interrupts owned work without ciphertext recovery or resend',
    { http: ['PostMessages', 'GetMessage'], wss: deliveryWSS },
    () => scenarioRestart(session, context),
  ));
  scenarios.push(await runDeliveryScenario(
    session,
    'temporary PostgreSQL outage yields uncertainty without a compensating send',
    { http: ['PostMessages'], wss: [] },
    () => scenarioPostgresOutage(session, context),
  ));
  scenarios.push(await runDeliveryScenario(
    session,
    'own retained metadata lookup grants no forwarding authority',
    { http: ['PostMessages', 'GetMessage'], wss: deliveryWSS },
    () => scenarioRetainedLookup(session, context),
  ));
  return { scenarios, requests: session.requestIds.length };
}
