// Package templates embeds the default templates and the artifacts they reference.
package templates

import (
	"embed"
	"io/fs"
	"path"
)

//go:embed *.json artifacts/*.json emulator/*.json emulator/artifacts/*.json
var files embed.FS

// Artifacts and Templates are the defaults arkd emulates: the leaves the daemon spends name its
// signer key tweaked by the arkade script.
var Artifacts, Templates = documents("artifacts/*.json"), documents("*.json")

// EmulatorArtifacts and EmulatorTemplates are the defaults a standalone emulator emulates: the
// leaves the daemon spends name arkd's signer key next to the emulator's key tweaked by the arkade
// script.
var (
	EmulatorArtifacts = documents("emulator/artifacts/*.json")
	EmulatorTemplates = documents("emulator/*.json")
)

func documents(pattern string) map[string][]byte {
	names, _ := fs.Glob(files, pattern)
	docs := map[string][]byte{}
	for _, name := range names {
		docs[path.Base(name)], _ = files.ReadFile(name)
	}
	return docs
}
