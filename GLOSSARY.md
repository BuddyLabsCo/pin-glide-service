# Pin Glide

Pin Glide connects an Account's Android Senders and Windows Receivers to deliver verification messages.

## Language

**Account**:
A user identity verified by one configured WeChat or QQ application, to which Sender and Receiver devices belong. An Account may have multiple Senders and Receivers.
_Avoid_: Sending account, receiving account

**Sender**:
An Android app installation registered to an Account that originates verification messages and manages its own Receiver associations.
_Avoid_: Phone, uploader

**Receiver**:
A Windows app installation registered to an Account that receives verification messages from associated Senders in that Account. A Receiver may have independent Pairings with multiple Senders.
_Avoid_: Desktop, target

**Receiver Presence**:
Whether a Receiver currently holds its one committed delivery connection, together with the time that published state last changed. A service restart begins with every Receiver offline.
_Avoid_: Ack, heartbeat, last seen

**Receiver Credential Generation**:
The monotonic number of a Receiver's current Receiver credential. A newer generation supersedes older credentials, so only the current generation authorizes delivery presence.
_Avoid_: Version, attempt

**Device Registration**:
The service identity of one app installation within an Account. Reinstalling the app creates a new Device Registration.
_Avoid_: Hardware identity, hardware fingerprint

**User Session**:
An Account owner's authenticated management context on one Sender.
_Avoid_: WeChat session, QQ session, upload authorization

**Refresh Operation**:
One ordered renewal of a Sender's credential bundle, identified by the client's
operation ID; the attempt number (0 original, 1 retransmission) ranks the
transmissions of that operation, so the service can recover one lost response
without replaying it.
_Avoid_: Login

**Pairing**:
A Sender-owned authorized association between one Sender and one Receiver. It grants the Sender permission to forward Messages to that Receiver.
_Avoid_: Binding, connection

**Pairing Session**:
A temporary authorization context between a Sender and a Receiver for forming a Pairing or authorizing recovery of Receiver access.
_Avoid_: User Session, Delivery

**Message**:
One verification message originating from a Sender, independent of the number of intended Receivers.
_Avoid_: Batch, Delivery

**Delivery**:
An attempt to forward one Message to one requested Receiver.
_Avoid_: Message, batch

**Receipt**:
A Receiver's acknowledgment that a Message has arrived, without asserting successful validation, decryption, display, or copying.
_Avoid_: Decryption success, display success

**Configuration Data**:
The service's authoritative identity, authorization, operational settings and release state, maintained through explicit business or deployment actions.
_Avoid_: Immutable data, diagnostic history

**Transaction Data**:
Records of Message delivery or temporary authentication and Pairing workflows, including their processing state and results.
_Avoid_: Database transaction, runtime log

**Log Data**:
Diagnostic records of execution and failures used to understand service operation, rather than to determine authority or whether work may execute again.
_Avoid_: Credential state, migration ledger, Delivery result

**Retention Policy**:
A table-level configuration identifying the records' time field and how long to retain them before scheduled cleanup.
_Avoid_: Per-message expiry, credential validity
