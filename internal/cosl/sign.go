package cosl

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	coslv1 "defios.com/proto/cosl/v1"
)

// ErrBadSignature is returned by Verify when the signature does not check.
var ErrBadSignature = errors.New("cosl: signature does not verify")

// canonical is the deterministic marshal every signature covers.
var canonical = proto.MarshalOptions{Deterministic: true}

// CanonicalBytes serializes env with sig unset (docs/spec/mvp.md §2.6).
func CanonicalBytes(env *coslv1.Envelope) ([]byte, error) {
	unsigned := proto.Clone(env).(*coslv1.Envelope)
	unsigned.Sig = nil
	return canonical.Marshal(unsigned)
}

// Sign sets env.Sig to the sender's Ed25519 signature over the canonical
// bytes and returns the full signed envelope bytes ready for the log.
func Sign(env *coslv1.Envelope, priv ed25519.PrivateKey) ([]byte, error) {
	msg, err := CanonicalBytes(env)
	if err != nil {
		return nil, fmt.Errorf("cosl: %w", err)
	}
	env.Sig = ed25519.Sign(priv, msg)
	signed, err := canonical.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("cosl: %w", err)
	}
	return signed, nil
}

// Unmarshal parses envelope bytes without verifying anything.
func Unmarshal(b []byte) (*coslv1.Envelope, error) {
	var env coslv1.Envelope
	if err := proto.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("cosl: envelope: %w", err)
	}
	return &env, nil
}

// Verify checks env.Sig against pub over the canonical bytes.
func Verify(env *coslv1.Envelope, pub ed25519.PublicKey) error {
	if len(env.Sig) != ed25519.SignatureSize || len(pub) != ed25519.PublicKeySize {
		return ErrBadSignature
	}
	msg, err := CanonicalBytes(env)
	if err != nil {
		return fmt.Errorf("cosl: %w", err)
	}
	if !ed25519.Verify(pub, msg, env.Sig) {
		return ErrBadSignature
	}
	return nil
}
