package coreclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// OperatorSigningPrefix is the domain separator of PROTOCOL.md §5.
const OperatorSigningPrefix = "intellectus/v1/operator\n"

// Envelope is the operator envelope of PROTOCOL.md §5.
type Envelope struct {
	Body      string `json:"body"`
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

// Operator signs operator bodies with an Ed25519 key. The body is
// marshalled exactly once; the signed bytes and the transmitted string are
// the same bytes.
//
// Body layout (assumption, see README): the op-specific fields are placed
// flat alongside the common fields, e.g.
//
//	{"issued_at":1700000000,"nonce":"...","op":"add_requirement",
//	 "project_id":"...","requirement_id":"R1","sensitivity":"internal","text":"..."}
type Operator struct {
	key       ed25519.PrivateKey
	projectID string
	// Now returns the issue time (overridable for tests).
	Now func() time.Time
	seq atomic.Uint64
}

// GenerateOperatorSeed creates a fresh 32-byte seed; returns seed and the
// hex public key (the key_id).
func GenerateOperatorSeed() (seed []byte, publicKeyHex string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	return priv.Seed(), hex.EncodeToString(pub), nil
}

// ParseSeedHex decodes a hex-encoded 32-byte seed (surrounding whitespace ok).
func ParseSeedHex(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("operator seed: %w", err)
	}
	if len(b) != ed25519.SeedSize {
		return nil, fmt.Errorf("operator seed: want %d bytes, got %d", ed25519.SeedSize, len(b))
	}
	return b, nil
}

// NewOperator builds a signer for projectID from a 32-byte seed.
func NewOperator(seed []byte, projectID string) (*Operator, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("operator seed: want %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return &Operator{key: ed25519.NewKeyFromSeed(seed), projectID: projectID, Now: time.Now}, nil
}

// KeyID returns the hex public key.
func (o *Operator) KeyID() string {
	return hex.EncodeToString(o.key.Public().(ed25519.PublicKey))
}

// ProjectID returns the project the operator signs for.
func (o *Operator) ProjectID() string { return o.projectID }

var reservedBodyKeys = map[string]bool{"project_id": true, "nonce": true, "issued_at": true, "op": true}

func (o *Operator) nonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("n%d-%s", o.seq.Add(1), hex.EncodeToString(b[:])), nil
}

// Body builds the exact body bytes for op (with a fresh nonce).
func (o *Operator) Body(op string, params map[string]any) ([]byte, error) {
	if op == "" {
		return nil, errors.New("operator: empty op")
	}
	body := make(map[string]any, len(params)+4)
	for k, v := range params {
		if reservedBodyKeys[k] {
			return nil, fmt.Errorf("operator: param %q is reserved", k)
		}
		body[k] = v
	}
	n, err := o.nonce()
	if err != nil {
		return nil, err
	}
	body["project_id"] = o.projectID
	body["nonce"] = n
	body["issued_at"] = o.Now().Unix()
	body["op"] = op
	return marshalCompact(body) // map keys are emitted sorted
}

// SignBody signs exact body bytes: Ed25519(key, prefix + body).
func (o *Operator) SignBody(body []byte) Envelope {
	msg := make([]byte, 0, len(OperatorSigningPrefix)+len(body))
	msg = append(msg, OperatorSigningPrefix...)
	msg = append(msg, body...)
	return Envelope{
		Body:      string(body),
		KeyID:     o.KeyID(),
		Signature: hex.EncodeToString(ed25519.Sign(o.key, msg)),
	}
}

// Envelope builds and signs a body for op; returns the envelope and the
// body bytes that were signed.
func (o *Operator) Envelope(op string, params map[string]any) (Envelope, []byte, error) {
	body, err := o.Body(op, params)
	if err != nil {
		return Envelope{}, nil, err
	}
	return o.SignBody(body), body, nil
}

// VerifyEnvelope checks an envelope's signature (utility for tests/tools;
// the core performs the authoritative check).
func VerifyEnvelope(env Envelope) error {
	pub, err := hex.DecodeString(env.KeyID)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("verify: malformed key_id")
	}
	sig, err := hex.DecodeString(env.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("verify: malformed signature")
	}
	msg := append([]byte(OperatorSigningPrefix), env.Body...)
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return errors.New("verify: signature does not verify")
	}
	return nil
}
