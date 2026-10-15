// Package model exposes the enabled model configuration and its temporary
// provider credentials over the service's HTTP surface.
package model

import (
	"time"

	domain "github.com/BuddyLabsCo/pin-glide-service/internal/model"
)

// TagModel groups the published model operations.
const TagModel = "model"

// inferencePath is the OpenAI-compatible path the adopted regional endpoint
// serves, which the Sender posts its text-only extraction request to.
const inferencePath = "/compatible-mode/v1"

// otpResponseFormat is the adopted strict extraction profile: exactly one
// required nullable-string otp and no other property. A null otp means the
// provider found none; the Sender still validates the field and rejects an
// empty, invalid or not-in-the-SMS value before uploading anything.
var otpResponseFormat = ModelResponseFormat{
	Type: "json_schema",
	JSONSchema: ModelResponseSchema{
		Name:   "otp_extraction",
		Strict: true,
		Schema: ModelOTPSchema{
			Type:                 "object",
			Properties:           ModelOTPProperties{OTP: ModelOTPField{Type: []string{"string", "null"}}},
			Required:             []string{"otp"},
			AdditionalProperties: false,
		},
	},
}

// ModelCredential is the accepted POST /v1/model/credentials success payload:
// one temporary provider credential and the configuration snapshot it was
// issued for. The raw credential appears only here and only to the Sender that
// holds upload authority.
type ModelCredential struct {
	Credential    ModelProviderKey   `json:"credential" doc:"Temporary provider credential the Sender calls Bailian with directly"`
	Configuration ModelConfiguration `json:"configuration" doc:"The one configuration snapshot this credential belongs to"`
}

// ModelProviderKey is one temporary provider credential with the provider's
// actual expiry, so the Sender caches it for exactly as long as the provider
// allows.
type ModelProviderKey struct {
	APIKey    string    `json:"apiKey" doc:"Raw temporary Bailian API key; send it only to the provider, never to this service"`
	ExpiresAt time.Time `json:"expiresAt" doc:"When the provider itself expires this key, in RFC 3339 UTC"`
}

// ModelConfiguration is the non-secret configuration the Sender needs to call
// the provider directly: which workspace serves it and which inference profile
// to use.
type ModelConfiguration struct {
	Provider  string         `json:"provider" enum:"bailian" doc:"The only model provider the adopted profile permits"`
	Region    string         `json:"region" enum:"cn-beijing" doc:"The adopted provider region; the service never switches it implicitly"`
	BaseURL   string         `json:"baseUrl" doc:"Workspace-specific OpenAI-compatible base URL the Sender appends /chat/completions to"`
	Inference ModelInference `json:"inference" doc:"Inference settings the Sender sends to the provider unchanged"`
}

// ModelInference is the adopted extraction profile: the model, thinking
// explicitly disabled because the provider enables it by default, and the
// strict structured-output format.
type ModelInference struct {
	Model          string              `json:"model" doc:"Model ID to request; the parent key is restricted to it"`
	EnableThinking bool                `json:"enable_thinking" doc:"Always false: thinking is disabled explicitly rather than by a prompt instruction"`
	ResponseFormat ModelResponseFormat `json:"response_format" doc:"Strict JSON Schema the provider must satisfy"`
}

// ModelResponseFormat is the OpenAI-compatible structured-output request. The
// service never falls back to the unconstrained JSON Object mode, because that
// would give up the schema's constraint.
type ModelResponseFormat struct {
	Type       string              `json:"type" enum:"json_schema" doc:"Structured-output mode"`
	JSONSchema ModelResponseSchema `json:"json_schema" doc:"The named schema and its enforcement"`
}

// ModelResponseSchema names and enforces the extraction schema.
type ModelResponseSchema struct {
	Name   string         `json:"name" doc:"Schema name reported by the provider"`
	Strict bool           `json:"strict" doc:"Whether the provider must satisfy the schema exactly"`
	Schema ModelOTPSchema `json:"schema" doc:"The extraction schema itself"`
}

// ModelOTPSchema constrains the extraction result to one required nullable
// string and rejects every additional property.
type ModelOTPSchema struct {
	Type                 string             `json:"type" enum:"object" doc:"The result is one JSON object"`
	Properties           ModelOTPProperties `json:"properties" doc:"The only property the result may carry"`
	Required             []string           `json:"required" doc:"Properties the result must carry"`
	AdditionalProperties bool               `json:"additionalProperties" doc:"Whether properties outside this schema are allowed"`
}

// ModelOTPProperties is the single-property result object.
type ModelOTPProperties struct {
	OTP ModelOTPField `json:"otp" required:"true" doc:"The extracted OTP, or null when the message holds none"`
}

// ModelOTPField is a nullable string: an absent OTP is null, never a missing
// property, an empty string or a numeric guess.
type ModelOTPField struct {
	Type []string `json:"type" enum:"string,null" doc:"Nullable string; null means no OTP was found"`
}

type credentialOutput struct {
	Body ModelCredential
}

// wireCredential maps one issued credential to the published payload. The
// configuration snapshot is mapped as read, and the adopted inference profile
// is stated here once so the published contract and the returned body cannot
// drift apart.
func wireCredential(issued domain.Issued) ModelCredential {
	return ModelCredential{
		Credential: ModelProviderKey{
			APIKey:    issued.Credential.APIKey,
			ExpiresAt: issued.Credential.ExpiresAt,
		},
		Configuration: ModelConfiguration{
			Provider: domain.ProviderBailian,
			Region:   domain.RegionBeijing,
			BaseURL:  domain.Endpoint(issued.Configuration.WorkspaceID, inferencePath),
			Inference: ModelInference{
				Model:          issued.Configuration.Model,
				EnableThinking: false,
				ResponseFormat: otpResponseFormat,
			},
		},
	}
}
