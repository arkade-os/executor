package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	key := strings.Repeat("01", 32)
	for _, f := range []struct {
		name     string
		env      map[string]string
		expected string
	}{
		{
			name: "both keys",
			env:  map[string]string{"BATCH_COSIGNER_KEY": key, "SEALING_KEY": key},
		},
		{
			name:     "no batch cosigner key",
			env:      map[string]string{"SEALING_KEY": key},
			expected: "EXECUTOR_BATCH_COSIGNER_KEY is required",
		},
		{
			name:     "no sealing key",
			env:      map[string]string{"BATCH_COSIGNER_KEY": key},
			expected: "EXECUTOR_SEALING_KEY is required",
		},
		{
			name:     "sealing key of the wrong size",
			env:      map[string]string{"BATCH_COSIGNER_KEY": key, "SEALING_KEY": "01"},
			expected: "EXECUTOR_SEALING_KEY must be 32 bytes hex",
		},
	} {
		t.Run(f.name, func(t *testing.T) {
			for _, name := range []string{
				"DATABASE_URL", "ARK_URL", "EMULATOR_URL", "ELECTRUM_URL",
			} {
				t.Setenv(envPrefix+name, "set")
			}
			for _, name := range []string{"BATCH_COSIGNER_KEY", "SEALING_KEY"} {
				t.Setenv(envPrefix+name, f.env[name])
			}
			cfg, err := Load()
			if f.expected != "" {
				require.EqualError(t, err, f.expected)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, cfg.BatchCosignerKey)
			require.NotNil(t, cfg.SealingKey)
		})
	}
}
