// Package render turns a stack into what Kubernetes deploys: one namespace
// per environment, a Deployment and a Service per service, an Ingress per
// routed service. The stack format names no orchestrator.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"garrison/control-plane/internal/stack"
)

// Names the system stack provides. They are part of the platform's public
// contract with rendered stacks; see platform/contracts.
const (
	TraefikEntrypoint = "websecure" // HTTPS entry point with the wildcard certificate
	SSOMiddleware     = "sso@file"  // forward-auth middleware backed by Keycloak
	// PullSecret is the Secret pods pull the installation's images with;
	// the deployer copies it into each app's namespace.
	PullSecret = "registry-pull"

	// Labels of every app object: its unit, app (the stack file's
	// name) and environment.
	LabelUnit        = "garrison.unit"
	LabelApp         = "garrison.app"
	LabelEnvironment = "garrison.environment"
	// LabelStack names the environment's stack, as system stacks do
	// (system.StackLabel): applying it again prunes what it no longer has.
	LabelStack = "garrison.io/stack"
	// LabelService names the service a pod belongs to, within its namespace.
	LabelService = "garrison.io/service"
	// LabelStored marks a Secret that holds a stored secret (secrets/ in the
	// unit's config) rather than a generated one.
	LabelStored = "garrison.stored"

	GPUResource = "nvidia.com/gpu" // what the NVIDIA device plugin offers per GPU

	// A private environment's proof that a request came through the
	// sign-in. Traefik adds the environment's ingress key to what it passes
	// on, in IngressHeader; the app has the key in IngressKeyEnv and
	// believes the sign-in's headers only with it. Apps of other
	// namespaces cannot reach it past Traefik (each environment's
	// NetworkPolicy); the key keeps it so if they ever could.
	IngressHeader = "X-Garrison-Ingress"
	IngressKeyEnv = "GARRISON_INGRESS_KEY"
)

type Options struct {
	Environment string // environment to render
	Domain      string // installation domain, e.g. example.com
	DataRoot    string // where the ZFS pool is mounted; defaults to DefaultDataRoot
	Commit      string // the environment's commit, for {{commit}} in images
	// IngressKey, if set, is the ingress key of a private environment (see
	// IngressHeader): letters and digits only. Empty: none.
	IngressKey string
	// Secrets are the unit's stored secrets this environment gets, by
	// name (secrets/<name>.yaml in its config, decrypted); every other
	// secret a service names has a generated value.
	Secrets map[string]Stored
	// Members are the unit's people, its admins and members, for
	// {{members}} in a service's env: their names, comma-separated.
	Members []string
	// MainAddress is the main machine's own address on the pod network, as
	// a CIDR of one address (10.42.0.0/32): Traefik's, on the host's
	// network there, as pods on other machines see it. Empty on one machine
	// alone, where the host's traffic to its pods always passes.
	MainAddress string
}

// Stored is the value of a stored secret, and its version, which pods carry
// as an annotation, so a new version restarts them.
type Stored struct {
	Value   string
	Version string // letters and digits, e.g. a hash of the encrypted file
}

// Registry is the installation's image registry: Gitea's, at git.<domain>.
func Registry(domain string) string { return "git." + domain }

// Image fills in an image's placeholders: {{registry}} is the installation's
// registry, {{commit}} the first 12 characters of the environment's commit,
// so a pipeline's image for each commit is deployed with that commit.
func Image(image, domain, commit string) string {
	if len(commit) > 12 {
		commit = commit[:12]
	}
	image = stack.RegistryPlaceholder.ReplaceAllLiteralString(image, Registry(domain))
	return stack.CommitPlaceholder.ReplaceAllLiteralString(image, commit)
}

// DefaultDataRoot is where the installer mounts the dataset that holds all
// units. The ZFS dataset name is this path without the leading slash.
const DefaultDataRoot = "/garrison/ns"

// StackName is the stack name of an environment. Hostnames are unique
// across the installation, so the environment's hostname is used.
func StackName(s *stack.Stack, env string) string { return s.Hostname(env) }

// Namespace is the Kubernetes namespace of an environment: app-<stack>,
// shortened with a hash where that would pass 63 characters.
func Namespace(s *stack.Stack, env string) string { return NamespaceOf(StackName(s, env)) }

// NamespaceOf is the namespace of the stack called name.
func NamespaceOf(name string) string {
	ns := "app-" + name
	if len(ns) <= 63 {
		return ns
	}
	sum := sha256.Sum256([]byte(name))
	return strings.TrimRight(ns[:50], "-") + "-" + hex.EncodeToString(sum[:4])
}

// DatasetPath is where an environment's own dataset is mounted on the host.
func DatasetPath(root string, s *stack.Stack, env, dataset string) string {
	return path.Join(root, s.Unit, "stacks", s.Hostname(env), dataset)
}

// SharePath is where a unit's shared library was mounted on the host,
// before shares became volumes of OpenEBS; ShareDataset is the dataset now.
func SharePath(root, unit, share string) string {
	return path.Join(root, unit, "shares", share)
}

// An environment's volume and a unit's share each are a ZFSVolume of OpenEBS
// ZFS LocalPV, whose dataset is <its parent>/<its name>, and whose name is
// unique among them: an environment's <stack>.<dataset>, a share's
// <unit>.shares.<share>. Units, stacks and datasets are named without dots.
// A PersistentVolume of each, in each namespace that mounts it, binds the
// claim there; the deployer writes both (apply/volumes.go).

// StorageClass is the StorageClass of the volumes (garrison/openebs).
const StorageClass = "garrison-zfs"

// VolumeName is the ZFSVolume, and the PersistentVolume, of an environment's dataset.
func VolumeName(s *stack.Stack, env, dataset string) string { return StackName(s, env) + "." + dataset }

// ShareVolume is the ZFSVolume of a unit's share.
func ShareVolume(unit, share string) string { return unit + ".shares." + share }

// SharePV is the PersistentVolume of a unit's share in the namespace ns.
func SharePV(unit, share, ns string) string { return ShareVolume(unit, share) + "." + ns }

// EnvParent is the dataset that holds an environment's datasets.
func EnvParent(root string, s *stack.Stack, env string) string {
	return strings.TrimPrefix(path.Join(root, s.Unit, "stacks", s.Hostname(env)), "/")
}

// ShareParent is the dataset that holds a unit's shares.
func ShareParent(root, unit string) string {
	return strings.TrimPrefix(path.Join(root, unit, "shares"), "/")
}

// VolumeDataset is the dataset of an environment's volume.
func VolumeDataset(root string, s *stack.Stack, env, dataset string) string {
	return EnvParent(root, s, env) + "/" + VolumeName(s, env, dataset)
}

// ShareDataset is the dataset of a unit's share.
func ShareDataset(root, unit, share string) string {
	return ShareParent(root, unit) + "/" + ShareVolume(unit, share)
}

// DataClaim and ShareClaim are the PersistentVolumeClaims of an
// environment's dataset and of a share, in the environment's namespace.
func DataClaim(dataset string) string { return "data-" + dataset }
func ShareClaim(share string) string  { return "share-" + share }

// ShareSize is the size of a share's volume, which sets no limit of its
// own: a share has no size in the stack file; its pool's free space is what
// it has.
const ShareSize = "1Ti"

type object = map[string]any

func meta(name, namespace string, labels map[string]string, annotations map[string]string) object {
	m := object{"name": name, "labels": labels}
	if namespace != "" {
		m["namespace"] = namespace
	}
	if len(annotations) > 0 {
		m["annotations"] = annotations
	}
	return m
}

// Kubernetes renders one environment of a stack as Kubernetes objects, a
// YAML document each, for kubectl apply: its namespace and NetworkPolicy,
// and per service a Deployment, a Service and, if routed, an Ingress (with
// the Middleware of its ingress key, in a private environment).
func Kubernetes(s *stack.Stack, o Options) ([]byte, error) {
	env, ok := s.Environments[o.Environment]
	if !ok {
		return nil, fmt.Errorf("stack %s has no environment %q", s.Name, o.Environment)
	}
	if o.Domain == "" {
		return nil, fmt.Errorf("domain is required")
	}
	if o.DataRoot == "" {
		o.DataRoot = DefaultDataRoot
	}
	ns := Namespace(s, o.Environment)
	stackName := StackName(s, o.Environment)
	labels := func(extra ...string) map[string]string {
		l := map[string]string{LabelUnit: s.Unit, LabelApp: s.Name, LabelEnvironment: o.Environment, LabelStack: stackName}
		for i := 0; i+1 < len(extra); i += 2 {
			l[extra[i]] = extra[i+1]
		}
		return l
	}
	from := []object{{"podSelector": object{}}}
	if o.MainAddress != "" {
		// Traefik, from the main machine, to pods on the others.
		from = append(from, object{"ipBlock": object{"cidr": o.MainAddress}})
	}
	objs := []object{NamespaceObject(s, o.Environment),
		// The environment's pods reach each other, as on a network of their
		// own; nothing else reaches them but Traefik, on the machine.
		{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
			"metadata": meta("stack", ns, labels(), nil),
			"spec": object{"podSelector": object{}, "policyTypes": []string{"Ingress"},
				"ingress": []object{{"from": from}}}},
	}
	for _, name := range s.ServiceNames() {
		svc := s.Services[name]
		var envVars []object
		vars := map[string]string{}
		host := s.Hostname(o.Environment) + "." + o.Domain
		for k, v := range svc.Env {
			v = strings.ReplaceAll(v, "{{url}}", "https://"+host+"/")
			v = strings.ReplaceAll(v, "{{host}}", host)
			v = strings.ReplaceAll(v, "{{members}}", strings.Join(o.Members, ","))
			// Kubernetes expands $(NAME) in env values; $$ keeps a $ as it is.
			vars[k] = strings.ReplaceAll(v, "$(", "$$(")
		}
		routed := s.ServiceHostname(o.Environment, name)
		key := ""
		if routed != "" && env.Private {
			key = o.IngressKey
		}
		if key != "" {
			vars[IngressKeyEnv] = key
		}
		for _, k := range sortedKeys(vars) {
			envVars = append(envVars, object{"name": k, "value": vars[k]})
		}

		container := object{"name": name, "image": Image(svc.Image, o.Domain, o.Commit)}
		if len(envVars) > 0 {
			container["env"] = envVars
		}
		var mounts, volumes []object
		for _, vol := range sortedKeys(svc.Volumes) {
			v := svc.Volumes[vol]
			claim := DataClaim(v.Dataset)
			if v.Share != "" {
				claim = ShareClaim(v.Share)
			}
			m := object{"name": "v-" + vol, "mountPath": v.MountPath(vol)}
			if v.Readonly {
				m["readOnly"] = true
			}
			mounts = append(mounts, m)
			volumes = append(volumes, object{"name": "v-" + vol, "persistentVolumeClaim": object{"claimName": claim}})
		}
		annotations := map[string]string{}
		if len(svc.Secrets) > 0 {
			var sources []object
			for _, sec := range svc.Secrets {
				sources = append(sources, object{"secret": object{"name": sec}})
				if st, ok := o.Secrets[sec]; ok {
					annotations["garrison.io/secret-"+sec] = st.Version
				}
			}
			mounts = append(mounts, object{"name": "secrets", "mountPath": "/run/secrets", "readOnly": true})
			volumes = append(volumes, object{"name": "secrets", "projected": object{"sources": sources}})
		}
		if len(mounts) > 0 {
			container["volumeMounts"] = mounts
		}
		limits := object{}
		if svc.Resources.Memory != "" {
			limits["memory"] = svc.Resources.Memory
		}
		for _, d := range svc.Devices {
			if d == "gpu" {
				limits[GPUResource] = 1
			}
		}
		if len(limits) > 0 {
			container["resources"] = object{"limits": limits}
		}
		if svc.Port > 0 {
			container["ports"] = []object{{"containerPort": svc.Port}}
		}
		pod := object{
			"containers":                   []object{container},
			"imagePullSecrets":             []object{{"name": PullSecret}},
			"automountServiceAccountToken": false,
			// No environment variables of every other Service in the namespace.
			"enableServiceLinks": false,
		}
		if len(volumes) > 0 {
			pod["volumes"] = volumes
		}
		podMeta := object{"labels": labels(LabelService, name)}
		if len(annotations) > 0 {
			podMeta["annotations"] = annotations
		}
		spec := object{
			"replicas": 1,
			"selector": object{"matchLabels": map[string]string{LabelService: name}},
			"template": object{"metadata": podMeta, "spec": pod},
		}
		if len(svc.Volumes) > 0 {
			// One pod on its data at a time.
			spec["strategy"] = object{"type": "Recreate"}
		}
		objs = append(objs, object{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta(name, ns, labels(LabelService, name), nil), "spec": spec})

		// The service's name in the namespace, as on a Swarm network: headless,
		// so it resolves to its pod whatever port is asked for.
		svcSpec := object{"clusterIP": "None", "selector": map[string]string{LabelService: name}}
		if svc.Port > 0 {
			svcSpec["ports"] = []object{{"name": "http", "port": svc.Port}}
		}
		objs = append(objs, object{"apiVersion": "v1", "kind": "Service", "metadata": meta(name, ns, labels(LabelService, name), nil), "spec": svcSpec})

		if routed == "" {
			continue
		}
		ingressAnn := map[string]string{"traefik.ingress.kubernetes.io/router.entrypoints": TraefikEntrypoint}
		if env.Private {
			mw := SSOMiddleware
			if key != "" {
				objs = append(objs, object{"apiVersion": "traefik.io/v1alpha1", "kind": "Middleware",
					"metadata": meta(name+"-ingress", ns, labels(), nil),
					"spec":     object{"headers": object{"customRequestHeaders": map[string]string{IngressHeader: key}}}})
				mw += "," + ns + "-" + name + "-ingress@kubernetescrd"
			}
			ingressAnn["traefik.ingress.kubernetes.io/router.middlewares"] = mw
		}
		objs = append(objs, object{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
			"metadata": meta(name, ns, labels(LabelService, name), ingressAnn),
			"spec": object{"rules": []object{{"host": routed + "." + o.Domain, "http": object{"paths": []object{{
				"path": "/", "pathType": "Prefix",
				"backend": object{"service": object{"name": name, "port": object{"name": "http"}}}}}}}}}})
	}
	// The claims of the environment's datasets and shares, each bound to its
	// PersistentVolume by name.
	claims := map[string]object{}
	for _, name := range s.ServiceNames() {
		for _, v := range s.Services[name].Volumes {
			if v.Share != "" {
				claims[ShareClaim(v.Share)] = claimObject(ShareClaim(v.Share), ns, SharePV(s.Unit, v.Share, ns), labels())
			} else {
				claims[DataClaim(v.Dataset)] = claimObject(DataClaim(v.Dataset), ns, VolumeName(s, o.Environment, v.Dataset), labels())
			}
		}
	}
	for _, c := range sortedKeys(claims) {
		objs = append(objs, claims[c])
	}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	for _, obj := range objs {
		if err := enc.Encode(obj); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// claimObject is a PersistentVolumeClaim bound to the PersistentVolume pv.
func claimObject(name, ns, pv string, labels map[string]string) object {
	return object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": meta(name, ns, labels, nil),
		"spec": object{"accessModes": []string{"ReadWriteOnce"}, "storageClassName": StorageClass, "volumeName": pv,
			"resources": object{"requests": object{"storage": ClaimRequest}}}}
}

// ClaimRequest is what every claim of a volume asks for: the least, so it
// binds its PersistentVolume of any size. A volume's size is its PV's
// capacity and its ZFSVolume's, which the deployer changes (apply); a claim
// could only grow.
const ClaimRequest = "1Mi"

// NamespaceObject is the environment's namespace, labelled with its unit,
// app, environment and stack.
func NamespaceObject(s *stack.Stack, env string) map[string]any {
	l := map[string]string{LabelUnit: s.Unit, LabelApp: s.Name, LabelEnvironment: env, LabelStack: StackName(s, env)}
	return object{"apiVersion": "v1", "kind": "Namespace", "metadata": meta(Namespace(s, env), "", l, nil)}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
