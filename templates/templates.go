// Package templates embeds what new repos start with: the config repo of a
// namespace, and what system's adds to it.
package templates

import (
	"embed"
	"io/fs"
	"slices"
	"strings"
)

// all: keeps the files whose names start with a dot, as .gitea/.
//
//go:embed namespace-config system-config all:app
var files embed.FS

// Namespace and System name the template folders.
const (
	Namespace = "namespace-config"
	System    = "system-config"
)

// App is the folder of the app templates.
const App = "app"

// Apps are the names of the app templates.
func Apps() []string {
	entries, _ := files.ReadDir(App)
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// AppFiles are an app template's files, with {{namespace}} and {{repo}}
// filled in; nil if there is no such template.
func AppFiles(name, namespace, repo string) map[string]string {
	if name == "" || strings.ContainsAny(name, "/.") || !slices.Contains(Apps(), name) {
		return nil
	}
	out := Files(App + "/" + name)
	for p, c := range out {
		out[p] = strings.NewReplacer("{{namespace}}", namespace, "{{repo}}", repo).Replace(c)
	}
	return out
}

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
