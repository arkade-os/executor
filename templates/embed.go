// Package templates embeds the default templates and the artifacts they reference.
package templates

import (
	"embed"
	"io/fs"
	"path"
)

//go:embed *.json artifacts/*.json
var files embed.FS

var Artifacts, Templates = documents("artifacts/*.json"), documents("*.json")

func documents(pattern string) map[string][]byte {
	names, _ := fs.Glob(files, pattern)
	docs := map[string][]byte{}
	for _, name := range names {
		docs[path.Base(name)], _ = files.ReadFile(name)
	}
	return docs
}
