// Package system deploys system stacks: plain Compose files, not the
// platform's stack format, because they are the platform itself.
package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"homecloud/controller/internal/apply"
)

// PoolRoot is where the ZFS pool is mounted. Bind mounts below it are datasets.
const PoolRoot = "/homecloud/"

type Stack struct {
	Name string // Swarm stack name
	Dir  string // directory holding compose.yaml and the files it refers to
}

// Hash fingerprints a stack's files. Swarm configs cannot change, so config
// names carry it (${CONFIG_HASH}); a changed file gives new config names. It
// must match the installer: sha256 of the regular files concatenated in byte
// order of their names, first 12 hex digits.
func Hash(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return "", err
		}
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// Datasets lists the bind-mount sources below PoolRoot in a compose file.
// Each is a ZFS dataset that must exist before the stack is deployed.
func Datasets(composeFile string) ([]string, error) {
	data, err := os.ReadFile(composeFile)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Services map[string]struct {
			Volumes []yaml.Node `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", composeFile, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, svc := range doc.Services {
		for _, v := range svc.Volumes {
			var src string
			switch v.Kind {
			case yaml.ScalarNode: // short syntax, source:target[:mode]
				src, _, _ = strings.Cut(v.Value, ":")
			case yaml.MappingNode:
				var long struct {
					Type   string `yaml:"type"`
					Source string `yaml:"source"`
				}
				if err := v.Decode(&long); err != nil {
					return nil, fmt.Errorf("%s: %w", composeFile, err)
				}
				if long.Type == "bind" {
					src = long.Source
				}
			}
			src = filepath.Clean(src)
			if strings.HasPrefix(src+"/", PoolRoot) && src+"/" != PoolRoot && !seen[src] {
				seen[src] = true
				out = append(out, src)
			}
		}
	}
	return out, nil
}

// Deploy creates the datasets a system stack mounts and deploys it to Swarm.
// env is passed to `docker stack deploy` for ${VAR} substitution, together
// with CONFIG_HASH.
func Deploy(ctx context.Context, run apply.Runner, s Stack, env []string) error {
	compose := filepath.Join(s.Dir, "compose.yaml")
	datasets, err := Datasets(compose)
	if err != nil {
		return err
	}
	for _, d := range datasets {
		if err := apply.EnsureDataset(ctx, run, d, ""); err != nil {
			return err
		}
	}
	hash, err := Hash(s.Dir)
	if err != nil {
		return err
	}
	env = append(append([]string{}, env...), "CONFIG_HASH="+hash)
	_, err = run(ctx, env, nil, "docker", "stack", "deploy", "--detach=true", "--resolve-image", "never", "--compose-file", compose, s.Name)
	return err
}

// Remove takes a system stack off the cluster. Its datasets are kept.
func Remove(ctx context.Context, run apply.Runner, name string) error {
	_, err := run(ctx, nil, nil, "docker", "stack", "rm", name)
	return err
}
