// Package render turns a stack into what an orchestrator deploys. The stack
// format names no orchestrator; Swarm is the only target in the MVP.
package render

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"homecloud/control-plane/internal/stack"
)

// Names the system stack provides. They are part of the platform's public
// contract with rendered stacks; see platform/contracts.
const (
	TraefikNetwork    = "traefik-public" // overlay network Traefik routes over
	TraefikEntrypoint = "websecure"      // HTTPS entry point with the wildcard certificate
	SSOMiddleware     = "sso@file"       // forward-auth middleware backed by Keycloak

	// Labels of every app service: its namespace, app (the stack file's
	// name) and environment.
	LabelNamespace   = "homecloud.namespace"
	LabelApp         = "homecloud.app"
	LabelEnvironment = "homecloud.environment"
	GPUResource      = "GPU" // generic resource the installer advertises per GPU

	// A private environment's proof that a request came through the
	// sign-in. Every routed service is on TraefikNetwork, so one app can
	// call another directly, past Traefik, and name any person in the
	// sign-in's headers. Traefik adds the environment's ingress key to what
	// it passes on, in IngressHeader; the app has the key in IngressKeyEnv
	// and believes the sign-in's headers only with it.
	IngressHeader = "X-Homecloud-Ingress"
	IngressKeyEnv = "HOMECLOUD_INGRESS_KEY"
)

type Options struct {
	Environment string // environment to render
	Domain      string // installation domain, e.g. example.com
	DataRoot    string // where the ZFS pool is mounted; defaults to DefaultDataRoot
	Commit      string // the environment's commit, for {{commit}} in images
	// IngressKey, if set, is the ingress key of a private environment (see
	// IngressHeader): letters and digits only. Empty: none.
	IngressKey string
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
// namespaces. The ZFS dataset name is this path without the leading slash.
const DefaultDataRoot = "/homecloud/ns"

// StackName is the Swarm stack name of an environment. Hostnames are unique
// across the installation, so the environment's hostname is used.
func StackName(s *stack.Stack, env string) string { return s.Hostname(env) }

// DatasetPath is where an environment's own dataset is mounted on the host.
func DatasetPath(root string, s *stack.Stack, env, dataset string) string {
	return path.Join(root, s.Namespace, "stacks", s.Hostname(env), dataset)
}

// SharePath is where a namespace's shared library is mounted on the host.
func SharePath(root, namespace, share string) string {
	return path.Join(root, namespace, "shares", share)
}

// SecretName is the Swarm secret holding an environment's secret.
func SecretName(s *stack.Stack, env, secret string) string {
	return s.Hostname(env) + "." + secret
}

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
	Networks map[string]external       `yaml:"networks,omitempty"`
	Secrets  map[string]external       `yaml:"secrets,omitempty"`
}

type external struct {
	External bool   `yaml:"external"`
	Name     string `yaml:"name,omitempty"`
}

type composeService struct {
	Image       string            `yaml:"image"`
	Environment map[string]string `yaml:"environment,omitempty"`
	Networks    []string          `yaml:"networks"`
	Volumes     []composeMount    `yaml:"volumes,omitempty"`
	Secrets     []composeSecret   `yaml:"secrets,omitempty"`
	Deploy      *composeDeploy    `yaml:"deploy,omitempty"`
}

type composeMount struct {
	Type     string `yaml:"type"`
	Source   string `yaml:"source"`
	Target   string `yaml:"target"`
	ReadOnly bool   `yaml:"read_only,omitempty"`
}

type composeSecret struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

type composeDeploy struct {
	Labels    map[string]string `yaml:"labels,omitempty"`
	Resources *composeResources `yaml:"resources,omitempty"`
}

type composeResources struct {
	Limits       *composeLimits       `yaml:"limits,omitempty"`
	Reservations *composeReservations `yaml:"reservations,omitempty"`
}

type composeLimits struct {
	Memory string `yaml:"memory"`
}

type composeReservations struct {
	GenericResources []genericResource `yaml:"generic_resources"`
}

type genericResource struct {
	Discrete discreteResource `yaml:"discrete_resource_spec"`
}

type discreteResource struct {
	Kind  string `yaml:"kind"`
	Value int    `yaml:"value"`
}

// Swarm renders one environment of a stack as a Compose file for
// `docker stack deploy`.
func Swarm(s *stack.Stack, o Options) ([]byte, error) {
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

	file := composeFile{Services: map[string]composeService{}}
	for _, name := range s.ServiceNames() {
		svc := s.Services[name]
		out := composeService{Image: Image(svc.Image, o.Domain, o.Commit), Networks: []string{"default"}}
		if len(svc.Env) > 0 {
			host := s.Hostname(o.Environment) + "." + o.Domain
			out.Environment = map[string]string{}
			for k, v := range svc.Env {
				v = strings.ReplaceAll(v, "{{url}}", "https://"+host+"/")
				v = strings.ReplaceAll(v, "{{host}}", host)
				// docker stack deploy reads $ as interpolation; values are literal.
				out.Environment[k] = strings.ReplaceAll(v, "$", "$$")
			}
		}
		// Who the service belongs to, for the deployer's metrics and alerts.
		deploy := composeDeploy{Labels: map[string]string{
			LabelNamespace:   s.Namespace,
			LabelApp:         s.Name,
			LabelEnvironment: o.Environment,
		}}

		if host := s.ServiceHostname(o.Environment, name); host != "" {
			out.Networks = append(out.Networks, TraefikNetwork)
			if file.Networks == nil {
				file.Networks = map[string]external{}
			}
			file.Networks[TraefikNetwork] = external{External: true}
			key := ""
			if env.Private {
				key = o.IngressKey
			}
			maps.Copy(deploy.Labels, routeLabels(host, o.Domain, svc.Port, env.Private, key))
			if key != "" {
				if out.Environment == nil {
					out.Environment = map[string]string{}
				}
				out.Environment[IngressKeyEnv] = key
			}
		}

		for _, vol := range sortedKeys(svc.Volumes) {
			v := svc.Volumes[vol]
			m := composeMount{Type: "bind", Target: v.MountPath(vol), ReadOnly: v.Readonly}
			if v.Share != "" {
				m.Source = SharePath(o.DataRoot, s.Namespace, v.Share)
			} else {
				m.Source = DatasetPath(o.DataRoot, s, o.Environment, v.Dataset)
			}
			out.Volumes = append(out.Volumes, m)
		}

		for _, sec := range svc.Secrets {
			out.Secrets = append(out.Secrets, composeSecret{Source: sec, Target: sec})
			if file.Secrets == nil {
				file.Secrets = map[string]external{}
			}
			file.Secrets[sec] = external{External: true, Name: SecretName(s, o.Environment, sec)}
		}

		var res composeResources
		if svc.Resources.Memory != "" {
			res.Limits = &composeLimits{Memory: dockerBytes(svc.Resources.Memory)}
		}
		for _, d := range svc.Devices {
			if d == "gpu" {
				res.Reservations = &composeReservations{[]genericResource{{discreteResource{GPUResource, 1}}}}
			}
		}
		if res.Limits != nil || res.Reservations != nil {
			deploy.Resources = &res
		}
		if deploy.Labels != nil || deploy.Resources != nil {
			out.Deploy = &deploy
		}
		file.Services[name] = out
	}
	return yaml.Marshal(file)
}

// routeLabels are the Traefik labels that route https://<host>.<domain> to a
// service. The router and service are named after the hostname, which is
// unique across the installation. With an ingress key, a middleware of the
// same name puts it into IngressHeader after the sign-in, in place of
// whatever a client sent there.
func routeLabels(host, domain string, port int, private bool, ingressKey string) map[string]string {
	router := "traefik.http.routers." + host
	labels := map[string]string{
		"traefik.enable":        "true",
		"traefik.swarm.network": TraefikNetwork,
		router + ".rule":        fmt.Sprintf("Host(`%s.%s`)", host, domain),
		router + ".entrypoints": TraefikEntrypoint,
		router + ".tls":         "true",
		"traefik.http.services." + host + ".loadbalancer.server.port": fmt.Sprint(port),
	}
	if private {
		labels[router+".middlewares"] = SSOMiddleware
	}
	if private && ingressKey != "" {
		name := host + "-ingress"
		labels["traefik.http.middlewares."+name+".headers.customrequestheaders."+IngressHeader] = ingressKey
		labels[router+".middlewares"] = SSOMiddleware + "," + name + "@swarm"
	}
	return labels
}

// dockerBytes converts a quantity such as 4Gi to Docker's spelling, 4g.
// Docker's k, m, g and t are powers of 1024, like Ki, Mi, Gi and Ti.
func dockerBytes(q string) string {
	return strings.ToLower(strings.TrimSuffix(q, "i"))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
