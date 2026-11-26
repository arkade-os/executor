package asm

const (
	OperatorKey      = "OPERATOR_KEY"
	BatchCosignerKey = "BATCH_COSIGNER_KEY"
	SealingKey       = "SEALING_KEY"
	EmulatorKey      = "EMULATOR_KEY"
)

var keyAliases = map[string]string{
	"SERVER_KEY": OperatorKey, "COSIGNER_KEY": BatchCosignerKey, "ENCRYPTION_KEY": SealingKey,
}

// ServiceKey returns the service key a placeholder name stands for, so that documents written
// with a former name of a key keep their id and their scripts. Any other name is returned as is.
func ServiceKey(name string) string {
	if key, ok := keyAliases[name]; ok {
		return key
	}
	return name
}
