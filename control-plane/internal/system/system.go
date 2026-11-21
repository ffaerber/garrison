// Package system deploys system stacks: plain Kubernetes manifests, not the
// platform's stack format, because they are the platform itself. A system
// stack is a directory with kubernetes.yaml, one or more documents, and the
// files its ConfigMaps take in. Everything runs in the namespace garrison.
package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"garrison/control-plane/internal/apply"
)

const (
	// Manifest is the file that makes a directory, or a repo of garrison, a
	// system stack. Files named *.kubernetes.yaml beside it are applied with
	// it, after it, e.g. a vendored CustomResourceDefinition.
	Manifest = "kubernetes.yaml"
	// Namespace is where the platform runs.
	Namespace = "garrison"
	// StackLabel names the stack an object belongs to. Every object of a
	// stack gets it, so applying the stack again prunes what it no longer has.
	StackLabel = "garrison.io/stack"
	// FilesAnnotation on a ConfigMap lists files of the stack's directory,
	// separated by spaces; each becomes a key of its data, named after it.
	FilesAnnotation = "garrison.io/files"
	// PoolRoot is where the ZFS pool is mounted. hostPath volumes below it
	// are datasets.
	PoolRoot = "/garrison/"
)

// Pruned are the kinds `kubectl apply --prune` may delete when a stack no
// longer has an object of them. Secrets are not among them: the installer
// makes those, and they carry no stack's label.
var Pruned = []string{
	"core/v1/Namespace", "node.k8s.io/v1/RuntimeClass",
	"admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy", "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicyBinding",
	"core/v1/ConfigMap", "core/v1/Service", "core/v1/ServiceAccount", "core/v1/PersistentVolumeClaim",
	"apps/v1/Deployment", "apps/v1/StatefulSet", "apps/v1/DaemonSet", "batch/v1/Job",
	"networking.k8s.io/v1/Ingress", "networking.k8s.io/v1/NetworkPolicy",
	"rbac.authorization.k8s.io/v1/Role", "rbac.authorization.k8s.io/v1/RoleBinding",
	"rbac.authorization.k8s.io/v1/ClusterRole", "rbac.authorization.k8s.io/v1/ClusterRoleBinding",
}

type Stack struct {
	Name string // the stack's name, its repo's
	Dir  string // directory holding kubernetes.yaml and the files it takes in
}

// Hash fingerprints a stack's files: sha256 of the regular files
// concatenated in byte order of their names, first 12 hex digits. Pod
// templates carry it as ${CONFIG_HASH}, so a changed file rolls their pods.
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

var variable = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// Render is the stack as it is applied: ${VAR} filled in from env (KEY=VALUE)
// and CONFIG_HASH, unknown ones left as they are; every object labelled with
// the stack; and the data of ConfigMaps that name files (FilesAnnotation)
// read from the stack's directory.
func Render(s Stack, env []string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, Manifest))
	if err != nil {
		return nil, err
	}
	more, err := filepath.Glob(filepath.Join(s.Dir, "*."+Manifest))
	if err != nil {
		return nil, err
	}
	for _, f := range more {
		extra, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		data = append(append(data, "\n---\n"...), extra...)
	}
	hash, err := Hash(s.Dir)
	if err != nil {
		return nil, err
	}
	vars := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	vars["CONFIG_HASH"] = hash
	data = variable.ReplaceAllFunc(data, func(m []byte) []byte {
		if v, ok := vars[string(m[2:len(m)-1])]; ok {
			return []byte(v)
		}
		return m
	})

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", s.Name, Manifest, err)
		}
		if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
			continue // an empty document
		}
		obj := doc.Content[0]
		if err := label(obj, s.Name); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", s.Name, Manifest, err)
		}
		if scalar(obj, "kind") == "ConfigMap" {
			if err := takeFiles(obj, s.Dir); err != nil {
				return nil, fmt.Errorf("%s/%s: %w", s.Name, Manifest, err)
			}
		}
		plain(&doc)
		if err := enc.Encode(&doc); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// clusterScoped are the kinds without a namespace that stacks use.
var clusterScoped = map[string]bool{
	"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true, "IngressClass": true, "RuntimeClass": true,
	"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true, "StorageClass": true,
	"PersistentVolume": true, "PriorityClass": true, "CustomResourceDefinition": true,
}

// label puts StackLabel=name into obj's metadata.labels, and Namespace into
// its metadata.namespace if it is namespaced and names none.
func label(obj *yaml.Node, name string) error {
	kind := scalar(obj, "kind")
	if kind == "" {
		return errors.New("a document without kind")
	}
	meta := child(obj, "metadata", yaml.MappingNode)
	if !clusterScoped[kind] && scalar(meta, "namespace") == "" {
		set(meta, "namespace", Namespace)
	}
	labels := child(meta, "labels", yaml.MappingNode)
	set(labels, StackLabel, name)
	// A workload's pods too, so they are found by their stack. Not added to
	// its selector, which cannot change.
	if tmpl := field(field(obj, "spec"), "template"); tmpl != nil && tmpl.Kind == yaml.MappingNode {
		set(child(child(tmpl, "metadata", yaml.MappingNode), "labels", yaml.MappingNode), StackLabel, name)
		// The platform's Deployments and StatefulSets run on the main
		// machine, where its data (hostPath under /garrison/system), Docker
		// and Traefik's address are. DaemonSets run on every machine.
		if kind == "Deployment" || kind == "StatefulSet" {
			set(child(child(tmpl, "spec", yaml.MappingNode), "nodeSelector", yaml.MappingNode), MainNodeLabel, "true")
		}
	}
	return nil
}

// MainNodeLabel is the label k3s gives its server, the main machine.
const MainNodeLabel = "node-role.kubernetes.io/control-plane"

// takeFiles fills a ConfigMap's data with the files its annotation names.
func takeFiles(obj *yaml.Node, dir string) error {
	meta := field(obj, "metadata")
	files := scalar(field(meta, "annotations"), FilesAnnotation)
	if files == "" {
		return nil
	}
	data := child(obj, "data", yaml.MappingNode)
	for _, f := range strings.Fields(files) {
		if f != filepath.Base(f) {
			return fmt.Errorf("ConfigMap %s: %s is not a file of the stack's directory", scalar(meta, "name"), f)
		}
		content, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return fmt.Errorf("ConfigMap %s: %w", scalar(meta, "name"), err)
		}
		set(data, f, string(content))
	}
	return nil
}

// Datasets lists the hostPath volumes below PoolRoot in rendered manifests.
// Each is a ZFS dataset that must exist before the stack is applied.
func Datasets(manifests []byte) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "hostPath" {
					p := filepath.Clean(scalar(n.Content[i+1], "path"))
					if strings.HasPrefix(p+"/", PoolRoot) && p+"/" != PoolRoot && !seen[p] {
						seen[p] = true
						out = append(out, p)
					}
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	dec := yaml.NewDecoder(bytes.NewReader(manifests))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		walk(&doc)
	}
	return out, nil
}

// Deploy creates the datasets a system stack mounts and applies it, in the
// namespace garrison unless an object names another, pruning what of it the
// stack no longer has. env is
// for ${VAR} substitution (Render). It does not wait for the rollout; Wait does.
func Deploy(ctx context.Context, run apply.Runner, s Stack, env []string) error {
	manifests, err := Render(s, env)
	if err != nil {
		return err
	}
	datasets, err := Datasets(manifests)
	if err != nil {
		return err
	}
	for _, d := range datasets {
		if err := apply.EnsureDataset(ctx, run, d, ""); err != nil {
			return err
		}
	}
	args := []string{"apply", "--filename", "-", "--prune", "--selector", StackLabel + "=" + s.Name}
	for _, k := range Pruned {
		args = append(args, "--prune-allowlist", k)
	}
	_, err = run(ctx, nil, manifests, "kubectl", args...)
	return err
}

// Wait waits until every Deployment, StatefulSet and DaemonSet of the stack
// has rolled out, for up to timeout (e.g. "600s").
func Wait(ctx context.Context, run apply.Runner, name, timeout string) error {
	out, err := run(ctx, nil, nil, "kubectl", "get", "deployments,statefulsets,daemonsets", "--namespace", Namespace,
		"--selector", StackLabel+"="+name, "--output", "name")
	if err != nil {
		return err
	}
	for _, w := range strings.Fields(string(out)) {
		if _, err := run(ctx, nil, nil, "kubectl", "rollout", "status", w, "--namespace", Namespace, "--timeout", timeout); err != nil {
			return fmt.Errorf("%s: %w", w, err)
		}
	}
	return nil
}

// Remove takes a system stack off the cluster. Its datasets are kept.
func Remove(ctx context.Context, run apply.Runner, name string) error {
	kinds := make([]string, len(Pruned))
	for i, k := range Pruned {
		parts := strings.Split(k, "/")
		kinds[i] = strings.ToLower(parts[len(parts)-1])
		if g := parts[0]; g != "core" && g != "apps" {
			kinds[i] += "." + g
		}
	}
	_, err := run(ctx, nil, nil, "kubectl", "delete", strings.Join(kinds, ","), "--all-namespaces",
		"--selector", StackLabel+"="+name, "--ignore-not-found")
	return err
}

// plain writes n in block style, as kubectl shows objects, whatever style
// its file used; scalars are quoted again only where YAML needs it. yaml.v3
// follows YAML 1.2, where yes and no are strings, but kubectl reads YAML
// 1.1, where they are booleans: such strings stay quoted (OpenEBS's CRDs
// allow "yes" and "no").
func plain(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode && n.ShortTag() == "!!str" && yaml11Bool[strings.ToLower(n.Value)] {
		n.Style = yaml.DoubleQuotedStyle
	} else if n.Style&yaml.LiteralStyle == 0 {
		n.Style = 0
	}
	for _, c := range n.Content {
		plain(c)
	}
}

// yaml11Bool are the words YAML 1.1 reads as booleans, in lower case.
var yaml11Bool = map[string]bool{"y": true, "yes": true, "n": true, "no": true, "on": true, "off": true, "true": true, "false": true}

// field is the value of key in the mapping n, or nil.
func field(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// scalar is the string value of key in the mapping n, or "".
func scalar(n *yaml.Node, key string) string {
	if v := field(n, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

// child is the value of key in n, made as an empty node of kind if missing.
func child(n *yaml.Node, key string, kind yaml.Kind) *yaml.Node {
	if v := field(n, key); v != nil {
		if v.Kind != kind { // e.g. labels: null
			*v = yaml.Node{Kind: kind}
		}
		return v
	}
	v := &yaml.Node{Kind: kind}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
	return v
}

// set sets key to the string value in the mapping n.
func set(n *yaml.Node, key, value string) {
	v := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	if len(value) > 0 && strings.Contains(value, "\n") {
		v.Style = yaml.LiteralStyle
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content[i+1] = v
			return
		}
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
}
