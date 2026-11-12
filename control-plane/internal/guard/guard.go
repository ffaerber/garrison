// Package guard is a Docker API proxy for the Actions runner of user
// namespaces. The runner needs Docker to run jobs in containers, but Docker's
// API is root on the machine, and a workflow can ask for container options
// (devices, capabilities, host namespaces) that escape any container. The
// guard lets through only what running a job needs, and refuses everything
// else:
//
//   - containers created only with plain settings: no privileges, added
//     capabilities, devices, security options, host namespaces, host mounts,
//     or other networks than the builds network;
//   - exec, copy, logs, start, stop and removal only of containers and execs
//     created through the guard, never of the platform's own;
//   - volumes only of the runner's own (by name prefix), without driver options;
//   - image pulls, but no builds, pushes, tags, commits or imports, and
//     nothing of Swarm, secrets, plugins or the system.
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
)

// Policy is what the guard allows.
type Policy struct {
	Network        string   // the only network job containers may join
	VolumePrefixes []string // names of volumes jobs may create and mount
}

// Guard proxies the Docker API on a Unix socket under a Policy.
type Guard struct {
	Policy Policy
	proxy  *httputil.ReverseProxy

	mu         sync.Mutex
	containers map[string]bool // ids and names of containers created through the guard
	execs      map[string]bool
}

// New proxies to the Docker daemon at socket.
func New(socket string, p Policy) *Guard {
	g := &Guard{Policy: p, containers: map[string]bool{}, execs: map[string]bool{}}
	target, _ := url.Parse("http://docker")
	g.proxy = httputil.NewSingleHostReverseProxy(target)
	g.proxy.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	g.proxy.ModifyResponse = g.record
	return g
}

var (
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
		log.Printf("guard: refused %s %s: %v", r.Method, r.URL.Path, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"message": "refused by the homecloud Docker guard: " + err.Error()})
		return
	}
	g.proxy.ServeHTTP(w, r)
}

func (g *Guard) owned(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.containers[id] || g.containers[strings.TrimPrefix(id, "/")]
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
		if !g.owned(mm[1]) {
			return fmt.Errorf("container %s was not created through the guard", mm[1])
		}
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
	HostConfig struct {
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
			Type   string
			Source string
		}
		VolumesFrom     []string
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
	for _, mt := range h.Mounts {
		add(mt.Type != "volume" && mt.Type != "tmpfs", mt.Type+" mount")
		add(mt.Type == "volume" && !g.volumeAllowed(mt.Source), "mount of "+mt.Source)
	}
	add(len(h.VolumesFrom) > 0, "volumes from another container")
	add(h.Runtime != "" && h.Runtime != "runc", "runtime "+h.Runtime)
	add(len(h.Sysctls) > 0, "sysctls")
	add(len(h.Links) > 0, "links")
	add(len(h.PortBindings) > 0 || h.PublishAllPorts, "published ports")
	add(h.OomKillDisable != nil && *h.OomKillDisable, "disabled OOM killer")
	add(h.Isolation != "" && h.Isolation != "default", "isolation "+h.Isolation)
	if len(bad) > 0 {
		return fmt.Errorf("container asks for %s", strings.Join(bad, ", "))
	}
	return nil
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
			if name := req.URL.Query().Get("name"); name != "" {
				g.containers[name] = true
			}
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
		if id, _ := c["Id"].(string); g.owned(id) {
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
