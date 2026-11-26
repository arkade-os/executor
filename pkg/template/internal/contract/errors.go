package contract

import "errors"

var (
	// ErrNoBatchCosigner marks a script naming BATCH_COSIGNER_KEY when that key is not given.
	ErrNoBatchCosigner = errors.New("no batch cosigner key")
	// ErrBadTree marks a given taptree that does not decode, repeats a leaf or misses a declared
	// leaf.
	ErrBadTree = errors.New("bad taptree")
	// ErrBadLeaf marks a leaf or covenant that does not assemble, is duplicated, or a definition
	// without leaves.
	ErrBadLeaf = errors.New("bad leaf")
)
