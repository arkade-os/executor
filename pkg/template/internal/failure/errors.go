// Package failure holds the error identities shared across packages.
package failure

import "errors"

var (
	ErrInvalidTemplate = errors.New("invalid template")
	// ErrUnsupported marks a valid template that a package cannot run in the requested mode.
	ErrUnsupported = errors.New("unsupported")
	// ErrInvalidVariables marks variables missing, extra or not in the canonical encoding of their
	// type.
	ErrInvalidVariables = errors.New("invalid variables")
	// ErrIneligible marks sources the template cannot spend as given.
	ErrIneligible = errors.New("ineligible")
	// ErrUnaffordable marks an ineligible source that fees or dust refuse, not a broken template.
	ErrUnaffordable       = errors.New("unaffordable")
	ErrNoMatch            = errors.New("transaction does not match the trigger")
	ErrUnsupportedVersion = errors.New("unsupported template version")

	ErrInvalidArtifact            = errors.New("invalid artifact")
	ErrUnsupportedArtifactVersion = errors.New("unsupported artifact version")
)
