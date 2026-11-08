package protocol

import "time"

// SenderRegistration identifies one unrevoked Sender without exposing credentials.
type SenderRegistration struct {
	SenderID   string     `json:"senderId" doc:"Sender registration ID; use this ID as the permanent-revocation target"`
	DeviceName string     `json:"deviceName" minLength:"1" maxLength:"64" doc:"Client-supplied trimmed device name; duplicate names identify distinct registrations by Sender ID"`
	State      string     `json:"state" enum:"active" doc:"Unrevoked registration; does not imply online, logged in or forwarding"`
	CreatedAt  time.Time  `json:"createdAt" doc:"Original registration creation time in RFC 3339 UTC"`
	RevokedAt  *time.Time `json:"revokedAt" nullable:"true" doc:"Always null for an unrevoked registration"`
}
