# Pin Glide

Pin Glide connects an Account's Android Senders and Windows Receivers to deliver verification messages.

## Language

**Account**:
A WeChat-backed user identity to which Sender and Receiver devices belong. An Account may have multiple Senders and Receivers.
_Avoid_: Sending account, receiving account

**Sender**:
An Android app installation registered to an Account that originates verification messages and manages its own Receiver associations.
_Avoid_: Phone, uploader

**Receiver**:
A Windows app installation registered to an Account that receives verification messages from associated Senders in that Account. A Receiver may have independent Pairings with multiple Senders.
_Avoid_: Desktop, target

**Device Registration**:
The service identity of one app installation within an Account. Reinstalling the app creates a new Device Registration.
_Avoid_: Hardware identity, hardware fingerprint

**User Session**:
An Account owner's authenticated management context on one Sender.
_Avoid_: WeChat session, upload authorization

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
