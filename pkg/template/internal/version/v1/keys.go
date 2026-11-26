package v1

import (
	"fmt"
	"strings"

	"github.com/arkade-os/executor/pkg/template/artifact"
	"github.com/arkade-os/executor/pkg/template/internal/asm"
	"github.com/arkade-os/executor/pkg/template/internal/failure"
	"github.com/arkade-os/executor/pkg/template/internal/model"
)

type keyRequirements uint8

const (
	operatorKey keyRequirements = 1 << iota
	emulatorKey
	batchCosignerKey
)

func (t *Template) checkKeys(keys model.Keys) error {
	switch {
	case t.requiredKeys&operatorKey != 0 && keys.Operator == nil:
		return fmt.Errorf("%w: missing operator key", failure.ErrUnsupported)
	case t.requiredKeys&emulatorKey != 0 && keys.Emulator == nil:
		return fmt.Errorf("%w: missing emulator key", failure.ErrUnsupported)
	case t.requiredKeys&batchCosignerKey != 0 && keys.BatchCosigner == nil:
		return fmt.Errorf("%w: missing batch cosigner key", failure.ErrUnsupported)
	}
	return nil
}

// keysRequired records service placeholders while compiling. Construction builds
// the entire contract tree, including unselected leaves.
func keysRequired(def *artifact.Definition) keyRequirements {
	var required keyRequirements
	tokens := func(script []string) {
		for _, tok := range script {
			m := asm.Placeholder.FindStringSubmatch(tok)
			if m == nil {
				continue
			}
			switch name := asm.ServiceKey(m[1]); {
			case name == asm.OperatorKey:
				required |= operatorKey
			case name == asm.BatchCosignerKey:
				required |= batchCosignerKey
			case strings.HasPrefix(name, asm.EmulatorKey+":"):
				required |= emulatorKey
			}
		}
	}
	for _, fn := range def.Functions {
		if fn.Arkade != nil {
			tokens(fn.Arkade.Asm)
		}
		for _, leaf := range fn.Leaves {
			tokens(leaf.Asm)
		}
	}
	return required
}
