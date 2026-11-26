package asm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServiceKey(t *testing.T) {
	for _, f := range []struct {
		name, given, want string
	}{
		{"operator", "OPERATOR_KEY", OperatorKey},
		{"former operator name", "SERVER_KEY", OperatorKey},
		{"batch cosigner", "BATCH_COSIGNER_KEY", BatchCosignerKey},
		{"former batch cosigner name", "COSIGNER_KEY", BatchCosignerKey},
		{"sealing", "SEALING_KEY", SealingKey},
		{"former sealing name", "ENCRYPTION_KEY", SealingKey},
		{"emulator", "EMULATOR_KEY", EmulatorKey},
		{"variable", "owner", "owner"},
		{"lower case former name", "server_key", "server_key"},
	} {
		t.Run(f.name, func(t *testing.T) {
			require.Equal(t, f.want, ServiceKey(f.given))
		})
	}
}
