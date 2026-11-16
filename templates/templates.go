// Package templates embeds what new repos start with: the config repo of a
// namespace, and what system's adds to it.
package templates

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed namespace-config system-config
var files embed.FS

// Namespace and System name the template folders.
const (
	Namespace = "namespace-config"
	System    = "system-config"
)

// Files are a template's files by their path in the repo.
func Files(dir string) map[string]string {
	out := map[string]string{}
	fs.WalkDir(files, dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(path, dir+"/")] = string(data)
		return nil
	})
	return out
}

// File is one file of a template; it panics if there is none, as the
// templates are compiled in.
func File(dir, path string) string {
	data, err := files.ReadFile(dir + "/" + path)
	if err != nil {
		panic(err)
	}
	return string(data)
}
