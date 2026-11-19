// Package guard is a Docker API proxy for the Actions runner of user
// namespaces, and for the agent service, which starts the agents' sandboxes. The runner needs Docker to run jobs in containers, but Docker's
// API is root on the machine, and a workflow can ask for container options
// (devices, capabilities, host namespaces) that escape any container. The
// guard lets through only what running a job needs, and refuses everything
// else:
//
//   - containers created only with plain settings: no privileges, added
//     capabilities, devices, security options, host namespaces, host mounts,
//     or other networks than the builds network;
//   - exec, copy, logs, start, stop and removal only of containers and execs
//     created through the guard, never of the platform's own. Docker finds a
//     container by id, name or the start of an id, so the guard asks which
//     one a request means and passes it on under that container's full id;
//   - volumes only of the runner's own (by name prefix), without driver
//     options, also where a mount would have Docker make the volume;
//   - image pulls, but no builds, pushes, tags, commits or imports, and
//     nothing of Swarm, secrets, plugins or the system.
//
// For sandboxes the policy is narrower still: only one image, every
// container labelled, with memory and process limits, and under gVisor. Containers are
// known by that label, so they stay manageable after the guard restarts.
package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Policy is what the guard allows.
type Policy struct {
	Network        string   // the only network job containers may join
	VolumePrefixes []string // names of volumes jobs may create and mount
	Image          string   // if set, the only image (repository, any tag) containers may run and be pulled
	Label          string   // if set, containers must carry this label as "true", and are owned by it
	MaxMemory      int64    // if set, containers must set a memory limit up to this and a process limit
	Runtime        string   // if set, the runtime containers must run under, e.g. runsc (gVisor); else runc
	// Share, if set, is the one host folder a container may bind, as
	// <root>/{namespace}/<rest>, e.g. /garrison/ns/{namespace}/shares/media:
	// only the namespace its NamespaceLabel names, and only at ShareTarget.
	Share          string
	ShareTarget    string
	NamespaceLabel string
}

// Guard proxies the Docker API on a Unix socket under a Policy.
type Guard struct {
	Policy Policy
	proxy  *httputil.ReverseProxy
	docker *http.Client

	mu         sync.Mutex
	containers map[string]bool // full ids of containers created through the guard
	execs      map[string]bool
}

// New proxies to the Docker daemon at socket.
func New(socket string, p Policy) *Guard {
	g := &Guard{Policy: p, containers: map[string]bool{}, execs: map[string]bool{}}
	target, _ := url.Parse("http://docker")
	g.proxy = httputil.NewSingleHostReverseProxy(target)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	g.proxy.Transport = transport
	g.docker = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	g.proxy.ModifyResponse = g.record
	return g
}

var (
	namespaceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	versionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+`)
	containerPath = regexp.MustCompile(`^/containers/([^/]+)(/(start|stop|kill|wait|attach|logs|archive|json|resize|exec|restart|top|stats))?$`)
	execPath      = regexp.MustCompile(`^/exec/([^/]+)/(start|json|resize)$`)
	volumePath    = regexp.MustCompile(`^/volumes/([^/]+)$`)
	imagePath     = regexp.MustCompile(`^/images/(.+)/json$`)
	networkPath   = regexp.MustCompile(`^/networks/([^/]+)$`)
)

func (g *Guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := versionPrefix.ReplaceAllString(r.URL.Path, "")
	if err := g.allow(r, path); err != nil {
		w.Header().Set("Content-Type", "application/json")
		if missing, ok := err.(noContainer); ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": missing.Error()})
			return
		}
		log.Printf("guard: refused %s %s: %v", r.Method, r.URL.Path, err)
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"message": "refused by the garrison Docker guard: " + err.Error()})
		return
	}
	g.proxy.ServeHTTP(w, r)
}

// noContainer: there is no container of that name, as Docker would say.
type noContainer string

func (n noContainer) Error() string { return "No such container: " + string(n) }

// created tells whether id is the full id of a container made through the guard.
func (g *Guard) created(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.containers[id]
}

// resolve finds the container ref means to Docker (a full id, a name, or
// the start of an id, in that order) and tells whether it is the guard's:
// created through it, or, under a label policy, carrying the label, as
// those created before the guard last started do. Names are not remembered:
// once a container is gone, its name can be the start of another's id.
func (g *Guard) resolve(ref string) (id string, found, owned bool) {
	if g.created(ref) {
		return ref, true, true
	}
	resp, err := g.docker.Get("http://docker/containers/" + url.PathEscape(ref) + "/json")
	if err != nil {
		return "", true, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, false
	}
	var c struct {
		Id     string
		Config struct{ Labels map[string]string }
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&c) != nil || c.Id == "" {
		return "", true, false
	}
	if g.created(c.Id) {
		return c.Id, true, true
	}
	if g.Policy.Label == "" || c.Config.Labels[g.Policy.Label] != "true" {
		return c.Id, true, false
	}
	g.mu.Lock()
	g.containers[c.Id] = true
	g.mu.Unlock()
	return c.Id, true, true
}

// imageAllowed: any image without an image policy, else only its repository.
func (g *Guard) imageAllowed(image string) bool {
	p := g.Policy.Image
	return p == "" || image == p || strings.HasPrefix(image, p+":") || strings.HasPrefix(image, p+"@")
}

func (g *Guard) allow(r *http.Request, path string) error {
	m := r.Method
	switch {
	case path == "/_ping" || path == "/version" || path == "/info":
		return nil
	case m == http.MethodGet && (path == "/images/json" || imagePath.MatchString(path)):
		return nil
	case m == http.MethodPost && path == "/images/create":
		if r.URL.Query().Get("fromImage") == "" || r.URL.Query().Get("fromSrc") != "" {
			return fmt.Errorf("only image pulls are allowed")
		}
		if !g.imageAllowed(r.URL.Query().Get("fromImage")) {
			return fmt.Errorf("image %s is not allowed", r.URL.Query().Get("fromImage"))
		}
		return nil
	case m == http.MethodGet && (path == "/networks" || networkPath.MatchString(path)):
		return nil
	case m == http.MethodGet && path == "/containers/json":
		return nil // the answer is cut down to the guard's own containers
	case m == http.MethodGet && path == "/volumes":
		return nil // the answer is cut down to job volumes
	case m == http.MethodPost && path == "/containers/create":
		return g.checkCreate(r)
	case m == http.MethodPost && path == "/volumes/create":
		return g.checkVolumeCreate(r)
	}
	if mm := volumePath.FindStringSubmatch(path); mm != nil && (m == http.MethodGet || m == http.MethodDelete) {
		if !g.volumeAllowed(mm[1]) {
			return fmt.Errorf("volume %s is not a job volume", mm[1])
		}
		return nil
	}
	if mm := containerPath.FindStringSubmatch(path); mm != nil {
		id, found, owned := g.resolve(mm[1])
		switch {
		case !found && g.Policy.Label != "":
			return noContainer(mm[1])
		case !owned:
			return fmt.Errorf("container %s was not created through the guard", mm[1])
		}
		// Docker gets the id found here, not the name again: by the time it
		// looked the name up itself, it could mean another container.
		r.URL.Path = strings.TrimSuffix(r.URL.Path, path) + "/containers/" + id + strings.TrimPrefix(path, "/containers/"+mm[1])
		r.URL.RawPath = ""
		if mm[3] == "exec" {
			return g.checkExec(r)
		}
		return nil
	}
	if mm := execPath.FindStringSubmatch(path); mm != nil {
		g.mu.Lock()
		ok := g.execs[mm[1]]
		g.mu.Unlock()
		if !ok {
			return fmt.Errorf("exec %s was not created through the guard", mm[1])
		}
		return nil
	}
	return fmt.Errorf("not needed to run jobs")
}

func readBody(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}

func (g *Guard) volumeAllowed(name string) bool {
	for _, p := range g.Policy.VolumePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

type createBody struct {
	Image      string
	Labels     map[string]string
	HostConfig struct {
		Memory            int64
		PidsLimit         *int64
		Privileged        bool
		CapAdd            []string
		Devices           []any
		DeviceRequests    []any
		DeviceCgroupRules []string
		SecurityOpt       []string
		PidMode           string
		IpcMode           string
		UTSMode           string
		UsernsMode        string
		CgroupnsMode      string
		CgroupParent      string
		NetworkMode       string
		Binds             []string
		Mounts            []struct {
			Type        string
			Source      string
			Target      string
			BindOptions *struct {
				Propagation      string
				NonRecursive     bool
				CreateMountpoint bool
			}
			VolumeOptions *struct {
				DriverConfig *struct {
					Name    string
					Options map[string]string
				}
			}
		}
		VolumesFrom     []string
		VolumeDriver    string
		MaskedPaths     []string
		ReadonlyPaths   []string
		Runtime         string
		Sysctls         map[string]string
		Links           []string
		PortBindings    map[string]any
		PublishAllPorts bool
		OomKillDisable  *bool
		Isolation       string
	}
	NetworkingConfig struct {
		EndpointsConfig map[string]any
	}
}

func (g *Guard) checkCreate(r *http.Request) error {
	var b createBody
	if err := readBody(r, &b); err != nil {
		return fmt.Errorf("unreadable container: %w", err)
	}
	h := b.HostConfig
	var bad []string
	add := func(cond bool, what string) {
		if cond {
			bad = append(bad, what)
		}
	}
	add(h.Privileged, "privileged")
	add(len(h.CapAdd) > 0, "added capabilities")
	add(len(h.Devices) > 0 || len(h.DeviceRequests) > 0 || len(h.DeviceCgroupRules) > 0, "devices")
	add(len(h.SecurityOpt) > 0, "security options")
	for name, mode := range map[string]string{"pid": h.PidMode, "ipc": h.IpcMode, "uts": h.UTSMode, "userns": h.UsernsMode, "cgroupns": h.CgroupnsMode} {
		add(mode != "" && mode != "private" && mode != "shareable" && mode != "none", name+" namespace "+mode)
	}
	add(h.CgroupParent != "", "cgroup parent")
	add(h.NetworkMode != g.Policy.Network, "network "+h.NetworkMode)
	for n := range b.NetworkingConfig.EndpointsConfig {
		add(n != g.Policy.Network, "network "+n)
	}
	for _, bind := range h.Binds {
		src, _, _ := strings.Cut(bind, ":")
		add(strings.HasPrefix(src, "/") || !g.volumeAllowed(src), "mount of "+src)
	}
	shares := 0
	for _, mt := range h.Mounts {
		if mt.Type == "bind" {
			shares++
			add(shares > 1 || !g.shareAllowed(mt.Source, mt.Target, b.Labels), "mount of "+mt.Source)
			if o := mt.BindOptions; o != nil {
				add((o.Propagation != "" && o.Propagation != "rprivate" && o.Propagation != "private") || o.CreateMountpoint, "bind options")
			}
			continue
		}
		add(mt.Type != "volume" && mt.Type != "tmpfs", mt.Type+" mount")
		add(mt.Type == "volume" && !g.volumeAllowed(mt.Source), "mount of "+mt.Source)
		// Docker makes a missing volume with these: type=none, o=bind and
		// device=/ would be the host's files under a job volume's name.
		if o := mt.VolumeOptions; o != nil && o.DriverConfig != nil {
			add((o.DriverConfig.Name != "" && o.DriverConfig.Name != "local") || len(o.DriverConfig.Options) > 0, "a volume with driver or driver options")
		}
	}
	add(len(h.VolumesFrom) > 0, "volumes from another container")
	add(h.VolumeDriver != "" && h.VolumeDriver != "local", "volume driver "+h.VolumeDriver)
	// Set, even to nothing, they replace Docker's own: /proc/sys and the
	// like would be writable, as with --security-opt systempaths=unconfined,
	// which the docker CLI turns into these two before it sends anything.
	add(h.MaskedPaths != nil || h.ReadonlyPaths != nil, "its own masked or read-only paths")
	if g.Policy.Runtime != "" {
		add(h.Runtime != g.Policy.Runtime, fmt.Sprintf("runtime %q, not %s", h.Runtime, g.Policy.Runtime))
	} else {
		add(h.Runtime != "" && h.Runtime != "runc", "runtime "+h.Runtime)
	}
	add(len(h.Sysctls) > 0, "sysctls")
	add(len(h.Links) > 0, "links")
	add(len(h.PortBindings) > 0 || h.PublishAllPorts, "published ports")
	add(h.OomKillDisable != nil && *h.OomKillDisable, "disabled OOM killer")
	add(h.Isolation != "" && h.Isolation != "default", "isolation "+h.Isolation)
	add(!g.imageAllowed(b.Image), "image "+b.Image)
	add(g.Policy.Label != "" && b.Labels[g.Policy.Label] != "true", "no label "+g.Policy.Label)
	if g.Policy.MaxMemory > 0 {
		add(h.Memory <= 0 || h.Memory > g.Policy.MaxMemory, fmt.Sprintf("memory %d", h.Memory))
		add(h.PidsLimit == nil || *h.PidsLimit <= 0, "no process limit")
	}
	if len(bad) > 0 {
		return fmt.Errorf("container asks for %s", strings.Join(bad, ", "))
	}
	return nil
}

// shareAllowed tells whether a container labelled labels may bind source
// at target: Policy.Share, for the namespace its label names, and nothing
// else.
func (g *Guard) shareAllowed(source, target string, labels map[string]string) bool {
	if g.Policy.Share == "" || g.Policy.NamespaceLabel == "" || target != g.Policy.ShareTarget {
		return false
	}
	ns := labels[g.Policy.NamespaceLabel]
	if !namespaceName.MatchString(ns) {
		return false
	}
	return source == strings.Replace(g.Policy.Share, "{namespace}", ns, 1)
}

func (g *Guard) checkExec(r *http.Request) error {
	var b struct{ Privileged bool }
	if err := readBody(r, &b); err != nil {
		return fmt.Errorf("unreadable exec: %w", err)
	}
	if b.Privileged {
		return fmt.Errorf("privileged exec")
	}
	return nil
}

func (g *Guard) checkVolumeCreate(r *http.Request) error {
	var b struct {
		Name       string
		Driver     string
		DriverOpts map[string]string
	}
	if err := readBody(r, &b); err != nil {
		return fmt.Errorf("unreadable volume: %w", err)
	}
	if !g.volumeAllowed(b.Name) {
		return fmt.Errorf("volume %q is not a job volume", b.Name)
	}
	if (b.Driver != "" && b.Driver != "local") || len(b.DriverOpts) > 0 {
		return fmt.Errorf("volume with driver or driver options")
	}
	return nil
}

// record remembers the containers and execs created through the guard, and
// forgets removed containers.
func (g *Guard) record(resp *http.Response) error {
	req := resp.Request
	path := versionPrefix.ReplaceAllString(req.URL.Path, "")
	switch {
	case req.Method == http.MethodPost && path == "/containers/create" && resp.StatusCode == http.StatusCreated:
		var out struct{ Id string }
		if err := peek(resp, &out); err == nil && out.Id != "" {
			g.mu.Lock()
			g.containers[out.Id] = true
			g.mu.Unlock()
		}
	case req.Method == http.MethodPost && strings.HasSuffix(path, "/exec") && resp.StatusCode == http.StatusCreated:
		var out struct{ Id string }
		if err := peek(resp, &out); err == nil && out.Id != "" {
			g.mu.Lock()
			g.execs[out.Id] = true
			g.mu.Unlock()
		}
	case req.Method == http.MethodGet && path == "/containers/json" && resp.StatusCode == http.StatusOK:
		return g.filterList(resp)
	case req.Method == http.MethodGet && path == "/volumes" && resp.StatusCode == http.StatusOK:
		return g.filterVolumes(resp)
	case req.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/") && resp.StatusCode < 300:
		g.mu.Lock()
		delete(g.containers, strings.TrimPrefix(path, "/containers/"))
		g.mu.Unlock()
	}
	return nil
}

func peek(resp *http.Response, v any) error {
	data, err := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// filterList keeps only the guard's own containers in a container list, so a
// job learns nothing about the platform's containers.
func (g *Guard) filterList(resp *http.Response) error {
	var all []map[string]any
	if err := peek(resp, &all); err != nil {
		resp.Body = io.NopCloser(strings.NewReader("[]"))
		resp.ContentLength = 2
		resp.Header.Set("Content-Length", "2")
		return nil
	}
	mine := []map[string]any{}
	for _, c := range all {
		labels, _ := c["Labels"].(map[string]any)
		if g.Policy.Label != "" {
			if labels[g.Policy.Label] == "true" {
				mine = append(mine, c)
			}
			continue
		}
		if id, _ := c["Id"].(string); g.created(id) {
			mine = append(mine, c)
		}
	}
	data, _ := json.Marshal(mine)
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Set("Content-Length", fmt.Sprint(len(data)))
	resp.Header.Del("Content-Encoding")
	return nil
}

// filterVolumes keeps only job volumes in a volume list.
func (g *Guard) filterVolumes(resp *http.Response) error {
	var all struct {
		Volumes  []map[string]any
		Warnings []string
	}
	_ = peek(resp, &all)
	mine := []map[string]any{}
	for _, v := range all.Volumes {
		if name, _ := v["Name"].(string); g.volumeAllowed(name) {
			mine = append(mine, v)
		}
	}
	data, _ := json.Marshal(map[string]any{"Volumes": mine, "Warnings": []string{}})
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Set("Content-Length", fmt.Sprint(len(data)))
	resp.Header.Del("Content-Encoding")
	return nil
}
