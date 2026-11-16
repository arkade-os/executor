package asm

import (
	"fmt"
	"regexp"

	"github.com/arkade-os/emulator/pkg/arkade"
	"github.com/btcsuite/btcd/btcec/v2"
)

const MaxScriptNum = 1<<53 - 1

// ScalarTypes are the types a variable, a constructor field or a witness item can have.
var ScalarTypes = []string{
	"pubkey", "signature", "bytes", "bytes20", "bytes32", "asset", "int", "bool",
}

var (
	DecimalTok  = regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)
	HexTok      = regexp.MustCompile(`^0x([0-9a-f]{2})+$`)
	Splice      = regexp.MustCompile(`^<!([^<>!]+)>$`)
	Placeholder = regexp.MustCompile(`^<([^<>]+)>$`)
)

func CanonicalBytes(typ string, raw []byte) error {
	var err error
	switch typ {
	case "pubkey":
		if _, e := btcec.ParsePubKey(raw); len(raw) != 33 || e != nil {
			err = fmt.Errorf("pubkey must be a valid 33-byte key")
		}
	case "signature":
		if len(raw) != 0 && len(raw) != 64 {
			err = fmt.Errorf("signature must be 64 bytes or empty")
		}
	case "bytes":
	case "bytes20":
		if len(raw) != 20 {
			err = fmt.Errorf("bytes20 must be 20 bytes")
		}
	case "bytes32":
		if len(raw) != 32 {
			err = fmt.Errorf("bytes32 must be 32 bytes")
		}
	case "asset":
		if len(raw) != 34 {
			err = fmt.Errorf("asset must be 34 bytes")
		}
	case "int":
		_, err = ParseScriptNum(raw)
	case "bool":
		if len(raw) > 1 || (len(raw) == 1 && raw[0] != 1) {
			err = fmt.Errorf("bool must be empty or 01")
		}
	default:
		err = fmt.Errorf("unknown type %q", typ)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadValue, err)
	}
	return nil
}

func ScriptNum(n int64) []byte {
	b, _ := arkade.BigNumFromInt64(n).Bytes()
	return b
}

// ParseScriptNum decodes a minimal Script number with magnitude up to 2^53-1.
func ParseScriptNum(b []byte) (int64, error) {
	n, err := arkade.MakeScriptNum(b, true, 8)
	if err != nil || n > MaxScriptNum || n < -MaxScriptNum {
		return 0, fmt.Errorf("%w: bad number of %d bytes", ErrBadValue, len(b))
	}
	return int64(n), nil
}
