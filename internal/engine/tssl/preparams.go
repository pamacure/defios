package tssl

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bnb-chain/tss-lib/v4/ecdsa/keygen"
)

// KeepOrGenerate returns existing ECDSA preparams unchanged. When existing
// is empty it generates tss-lib LocalPreParams, logs once, and returns the
// JSON adapter bytes stored in the Vault.
func KeepOrGenerate(existing []byte, logf func(string, ...any)) ([]byte, error) {
	if len(existing) > 0 {
		return append([]byte(nil), existing...), nil
	}
	if logf != nil {
		logf("generating ECDSA preparams")
	}
	pp, err := keygen.GeneratePreParamsWithContext(context.Background())
	if err != nil {
		return nil, fmt.Errorf("ecdsa preparams: %w", err)
	}
	if pp == nil || !pp.ValidateWithProof() {
		return nil, fmt.Errorf("ecdsa preparams failed validation")
	}
	raw, err := json.Marshal(pp)
	if err != nil {
		return nil, fmt.Errorf("ecdsa preparams: %w", err)
	}
	return raw, nil
}
