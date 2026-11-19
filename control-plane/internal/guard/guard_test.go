package guard

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeDocker answers on a Unix socket like Docker would and counts what reached it.
func fakeDocker(t *testing.T) (string, *[]string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/volumes"):
			io.WriteString(w, `{"Volumes":[{"Name":"GITEA-ACTIONS-TASK-1"},{"Name":"garrison-buildkit"}],"Warnings":null}`)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			io.WriteString(w, `[{"Id":"job1","Names":["/GITEA-ACTIONS-TASK-1_job"]},{"Id":"gitea1","Names":["/gitea_gitea.1"]}]`)
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"job1"}`)
		case strings.HasSuffix(r.URL.Path, "/containers/GITEA-ACTIONS-TASK-1_job/json"):
			io.WriteString(w, `{"Id":"job1"}`)
		case strings.HasSuffix(r.URL.Path, "/containers/gitea_gitea.1/json"), strings.HasSuffix(r.URL.Path, "/containers/control-plane_deployer.1/json"):
			io.WriteString(w, `{"Id":"gitea1"}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"exec1"}`)
		default:
			io.WriteString(w, "{}")
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close(); os.Remove(sock) })
	return sock, &seen
}

func setup(t *testing.T) (*httptest.Server, *[]string) {
	sock, seen := fakeDocker(t)
	g := New(sock, Policy{Network: "garrison-builds", VolumePrefixes: []string{"GITEA-ACTIONS-", "act-toolcache"}})
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return srv, seen
}

func do(t *testing.T, srv *httptest.Server, method, path string, body any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func job(host map[string]any) map[string]any {
	h := map[string]any{"NetworkMode": "garrison-builds", "Binds": []string{"GITEA-ACTIONS-TASK-1:/workspace", "act-toolcache:/toolcache"}}
	for k, v := range host {
		h[k] = v
	}
	return map[string]any{"Image": "golang:1.26-alpine", "HostConfig": h}
}

func TestAJobRuns(t *testing.T) {
	srv, _ := setup(t)
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"GET", "/_ping", nil, 200},
		{"POST", "/v1.47/images/create?fromImage=golang&tag=1.26-alpine", nil, 200},
		{"POST", "/v1.47/volumes/create", map[string]any{"Name": "GITEA-ACTIONS-TASK-1"}, 200},
		{"POST", "/v1.47/containers/create?name=GITEA-ACTIONS-TASK-1_job", job(nil), 201},
		{"POST", "/v1.47/containers/job1/start", nil, 200},
		{"PUT", "/v1.47/containers/job1/archive?path=/workspace", nil, 200},
		{"POST", "/v1.47/containers/job1/exec", map[string]any{"Cmd": []string{"go", "test"}}, 201},
		{"POST", "/v1.47/exec/exec1/start", map[string]any{}, 200},
		{"GET", "/v1.47/exec/exec1/json", nil, 200},
		{"GET", "/v1.47/containers/GITEA-ACTIONS-TASK-1_job/json", nil, 200},
		{"DELETE", "/v1.47/containers/job1?force=1", nil, 200},
		{"DELETE", "/v1.47/volumes/GITEA-ACTIONS-TASK-1", nil, 200},
	} {
		if got := do(t, srv, c.method, c.path, c.body); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, got, c.want)
		}
	}
}

func TestEscapesAreRefused(t *testing.T) {
	srv, seen := setup(t)
	before := len(*seen)
	for name, host := range map[string]map[string]any{
		"privileged":          {"Privileged": true},
		"capability":          {"CapAdd": []string{"SYS_ADMIN"}},
		"device":              {"Devices": []any{map[string]string{"PathOnHost": "/dev/sda"}}},
		"apparmor off":        {"SecurityOpt": []string{"apparmor=unconfined"}},
		"host pid":            {"PidMode": "host"},
		"host ipc":            {"IpcMode": "host"},
		"host userns":         {"UsernsMode": "host"},
		"other container pid": {"PidMode": "container:gitea"},
		"host network":        {"NetworkMode": "host"},
		"system network":      {"NetworkMode": "garrison-system"},
		"host bind":           {"Binds": []string{"/:/host"}},
		"docker socket":       {"Binds": []string{"/var/run/docker.sock:/var/run/docker.sock"}},
		"foreign volume":      {"Binds": []string{"gitea-data:/data"}},
		"bind mount":          {"Mounts": []any{map[string]string{"Type": "bind", "Source": "/etc"}}},
		"volumes from":        {"VolumesFrom": []string{"gitea_gitea.1"}},
		"other runtime":       {"Runtime": "sysbox-runc"},
		"sysctl":              {"Sysctls": map[string]string{"kernel.core_pattern": "|/x"}},
		"published port":      {"PortBindings": map[string]any{"80/tcp": []any{}}},
		"cgroup parent":       {"CgroupParent": "/"},
		"host files as a job volume": {"Mounts": []any{map[string]any{"Type": "volume", "Source": "GITEA-ACTIONS-x", "Target": "/host",
			"VolumeOptions": map[string]any{"DriverConfig": map[string]any{"Name": "local", "Options": map[string]string{"type": "none", "o": "bind", "device": "/"}}}}}},
		"volume driver":      {"VolumeDriver": "sshfs"},
		"unmasked /proc":     {"MaskedPaths": []string{}, "ReadonlyPaths": []string{}},
		"writable /proc/sys": {"ReadonlyPaths": []string{"/proc/bus"}},
	} {
		if got := do(t, srv, "POST", "/v1.47/containers/create", job(host)); got != 403 {
			t.Errorf("%s: %d, want 403", name, got)
		}
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1.47/build?t=postgres:17-alpine", nil},
		{"POST", "/v1.47/images/postgres/tag?repo=git.example.com/garrison/control-plane", nil},
		{"POST", "/v1.47/images/x/push", nil},
		{"POST", "/v1.47/commit?container=job1", nil},
		{"POST", "/v1.47/images/load", nil},
		{"POST", "/v1.47/images/create?fromSrc=-", nil},
		{"POST", "/v1.47/containers/gitea_gitea.1/exec", map[string]any{"Cmd": []string{"sh"}}},
		{"GET", "/v1.47/containers/control-plane_deployer.1/archive?path=/run/secrets", nil},
		{"POST", "/v1.47/exec/someone-elses/start", nil},
		{"GET", "/v1.47/services", nil},
		{"GET", "/v1.47/secrets", nil},
		{"POST", "/v1.47/swarm/join", nil},
		{"POST", "/v1.47/plugins/pull", nil},
		{"POST", "/v1.47/volumes/create", map[string]any{"Name": "GITEA-ACTIONS-x", "DriverOpts": map[string]string{"type": "none", "o": "bind", "device": "/"}}},
		{"POST", "/v1.47/volumes/create", map[string]any{"Name": "anything"}},
		{"DELETE", "/v1.47/volumes/garrison-data", nil},
		{"POST", "/v1.47/networks/create", map[string]any{"Name": "x"}},
	} {
		if got := do(t, srv, c.method, c.path, c.body); got != 403 {
			t.Errorf("%s %s: %d, want 403", c.method, c.path, got)
		}
	}
	// Nothing but the guard's own question, which container a name means.
	reached := slices.DeleteFunc((*seen)[before:], func(s string) bool {
		return strings.HasPrefix(s, "GET /containers/") && strings.HasSuffix(s, "/json")
	})
	if len(reached) != 0 {
		t.Errorf("refused requests reached Docker: %v", reached)
	}
	// A privileged exec, even into the job's own container, is refused.
	do(t, srv, "POST", "/v1.47/containers/create", job(nil))
	if got := do(t, srv, "POST", "/v1.47/containers/job1/exec", map[string]any{"Privileged": true}); got != 403 {
		t.Errorf("privileged exec: %d", got)
	}
}

func TestContainerListShowsOnlyJobs(t *testing.T) {
	srv, _ := setup(t)
	do(t, srv, "POST", "/v1.47/containers/create", job(nil))
	resp, err := http.Get(srv.URL + "/v1.47/containers/json?all=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0]["Id"] != "job1" {
		t.Errorf("list = %v", list)
	}
}

func TestVolumeListShowsOnlyJobVolumes(t *testing.T) {
	srv, _ := setup(t)
	resp, err := http.Get(srv.URL + "/v1.47/volumes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Volumes []map[string]any }
	json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Volumes) != 1 || out.Volumes[0]["Name"] != "GITEA-ACTIONS-TASK-1" {
		t.Errorf("volumes = %v", out.Volumes)
	}
}

func sandboxGuard(t *testing.T) (*httptest.Server, *[]string) {
	sock, seen := fakeDocker(t)
	g := New(sock, Policy{Network: "garrison-sandboxes", VolumePrefixes: []string{"garrison-agent-"},
		Image: "git.example.com/garrison/sandbox", Label: "garrison.sandbox", MaxMemory: 2 << 30, Runtime: "runsc",
		Share: "/garrison/ns/{namespace}/shares/media", ShareTarget: "/data/media", NamespaceLabel: "garrison.namespace"})
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return srv, seen
}

func sandboxSpec(change func(c, h map[string]any)) map[string]any {
	h := map[string]any{"NetworkMode": "garrison-sandboxes", "Memory": 1 << 30, "PidsLimit": 512, "Runtime": "runsc",
		"Mounts": []map[string]any{{"Type": "volume", "Source": "garrison-agent-1", "Target": "/data"}}}
	c := map[string]any{"Image": "git.example.com/garrison/sandbox:abc", "Labels": map[string]string{"garrison.sandbox": "true"}, "HostConfig": h}
	if change != nil {
		change(c, h)
	}
	return c
}

func TestOnlySandboxesAreCreated(t *testing.T) {
	srv, _ := sandboxGuard(t)
	if code := do(t, srv, "POST", "/v1.47/containers/create?name=garrison-agent-1", sandboxSpec(nil)); code != http.StatusCreated {
		t.Fatalf("a sandbox: %d", code)
	}
	for name, change := range map[string]func(c, h map[string]any){
		"other image":     func(c, h map[string]any) { c["Image"] = "alpine:3.22" },
		"lookalike image": func(c, h map[string]any) { c["Image"] = "git.example.com/garrison/sandbox-evil:1" },
		"no label":        func(c, h map[string]any) { delete(c, "Labels") },
		"no memory limit": func(c, h map[string]any) { delete(h, "Memory") },
		"too much memory": func(c, h map[string]any) { h["Memory"] = 8 << 30 },
		"no pids limit":   func(c, h map[string]any) { delete(h, "PidsLimit") },
		"other network":   func(c, h map[string]any) { h["NetworkMode"] = "garrison-system" },
		"other volume": func(c, h map[string]any) {
			h["Mounts"] = []map[string]any{{"Type": "volume", "Source": "gitea-data", "Target": "/data"}}
		},
		"docker socket":  func(c, h map[string]any) { h["Binds"] = []string{"/var/run/docker.sock:/var/run/docker.sock"} },
		"privileged":     func(c, h map[string]any) { h["Privileged"] = true },
		"without gVisor": func(c, h map[string]any) { delete(h, "Runtime") },
		"under runc":     func(c, h map[string]any) { h["Runtime"] = "runc" },
	} {
		if code := do(t, srv, "POST", "/v1.47/containers/create", sandboxSpec(change)); code != http.StatusForbidden {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code := do(t, srv, "POST", "/v1.47/images/create?fromImage=git.example.com/garrison/sandbox&tag=abc", nil); code != http.StatusOK {
		t.Errorf("pulling the sandbox image: %d", code)
	}
	if code := do(t, srv, "POST", "/v1.47/images/create?fromImage=alpine&tag=3.22", nil); code != http.StatusForbidden {
		t.Errorf("pulling another image: %d", code)
	}
}

func TestSandboxesAreKnownByTheirLabel(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv0 := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			io.WriteString(w, `[{"Id":"sb1","Labels":{"garrison.sandbox":"true"}},{"Id":"gitea1","Labels":{}}]`)
		case strings.HasSuffix(r.URL.Path, "/containers/sb1/json"):
			io.WriteString(w, `{"Id":"sb1","Config":{"Labels":{"garrison.sandbox":"true"}}}`)
		case strings.HasSuffix(r.URL.Path, "/containers/gitea1/json"):
			io.WriteString(w, `{"Id":"gitea1","Config":{"Labels":{}}}`)
		case strings.HasSuffix(r.URL.Path, "/containers/gone/json"), strings.HasSuffix(r.URL.Path, "/containers/gone"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})}
	go srv0.Serve(l)
	t.Cleanup(func() { srv0.Close() })
	// A guard that has just started, and created nothing yet.
	srv := httptest.NewServer(New(sock, Policy{Network: "n", Image: "img", Label: "garrison.sandbox", MaxMemory: 1}))
	t.Cleanup(srv.Close)
	if code := do(t, srv, "DELETE", "/v1.47/containers/sb1?force=1", nil); code != http.StatusNoContent {
		t.Errorf("removing a sandbox: %d", code)
	}
	if code := do(t, srv, "DELETE", "/v1.47/containers/gone?force=1", nil); code != http.StatusNotFound {
		t.Errorf("removing a container that is not there: %d", code)
	}
	if code := do(t, srv, "DELETE", "/v1.47/containers/gitea1?force=1", nil); code != http.StatusForbidden {
		t.Errorf("removing gitea: %d", code)
	}
	resp, err := http.Get(srv.URL + "/v1.47/containers/json?all=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0]["Id"] != "sb1" {
		t.Errorf("list %v", list)
	}
}

// Docker finds a container by name before it tries the start of an id. A
// name the guard once let through must not reach another container whose id
// starts with it, once the job's own container of that name is gone; and
// what the guard lets through goes to Docker under the full id it checked.
func TestANameIsNotRememberedAfterItsContainer(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	gone := false // the job's container, named ab, was removed
	srv0 := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"1111job"}`)
		case r.Method == "DELETE" && strings.HasSuffix(r.URL.Path, "/containers/1111job"):
			gone = true
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/containers/ab/json") && !gone:
			io.WriteString(w, `{"Id":"1111job"}`) // by name
		case strings.HasSuffix(r.URL.Path, "/containers/ab/json"):
			io.WriteString(w, `{"Id":"ab99gitea"}`) // by the start of Gitea's id
		case strings.HasSuffix(r.URL.Path, "/exec"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"exec1"}`)
		default:
			io.WriteString(w, "{}")
		}
	})}
	go srv0.Serve(l)
	t.Cleanup(func() { srv0.Close() })
	srv := httptest.NewServer(New(sock, Policy{Network: "garrison-builds", VolumePrefixes: []string{"GITEA-ACTIONS-", "act-toolcache"}}))
	t.Cleanup(srv.Close)

	if code := do(t, srv, "POST", "/v1.47/containers/create?name=ab", job(nil)); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if code := do(t, srv, "POST", "/v1.47/containers/ab/exec", map[string]any{"Cmd": []string{"true"}}); code != http.StatusCreated {
		t.Errorf("exec in the job by its name: %d", code)
	}
	mu.Lock()
	if last := seen[len(seen)-1]; last != "POST /v1.47/containers/1111job/exec" {
		t.Errorf("Docker got %q, not the id the guard checked", last)
	}
	mu.Unlock()
	if code := do(t, srv, "DELETE", "/v1.47/containers/1111job?force=1", nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if code := do(t, srv, "POST", "/v1.47/containers/ab/exec", map[string]any{"Cmd": []string{"sh"}}); code != http.StatusForbidden {
		t.Errorf("exec in the container whose id starts with the old name: %d, want 403", code)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range seen {
		if strings.Contains(s, "ab99gitea") || s == "POST /v1.47/containers/ab/exec" {
			t.Errorf("reached Docker: %s", s)
		}
	}
}

// A sandbox of a namespace's channel may bind that namespace's media
// library, at /data/media, and nothing else of the host.
func TestASandboxGetsItsNamespacesMedia(t *testing.T) {
	srv, _ := sandboxGuard(t)
	media := func(ns, source, target string, opts map[string]any) func(c, h map[string]any) {
		return func(c, h map[string]any) {
			c["Labels"] = map[string]string{"garrison.sandbox": "true", "garrison.namespace": ns}
			m := map[string]any{"Type": "bind", "Source": source, "Target": target}
			if opts != nil {
				m["BindOptions"] = opts
			}
			h["Mounts"] = append(h["Mounts"].([]map[string]any), m)
		}
	}
	if code := do(t, srv, "POST", "/v1.47/containers/create?name=garrison-agent-2",
		sandboxSpec(media("todo", "/garrison/ns/todo/shares/media", "/data/media", nil))); code != http.StatusCreated {
		t.Fatalf("todo's media: %d", code)
	}
	for name, change := range map[string]func(c, h map[string]any){
		"another namespace's":  media("todo", "/garrison/ns/lab/shares/media", "/data/media", nil),
		"another share":        media("todo", "/garrison/ns/todo/shares/photos", "/data/media", nil),
		"another target":       media("todo", "/garrison/ns/todo/shares/media", "/data", nil),
		"the host's root":      media("todo", "/", "/data/media", nil),
		"a path out":           media("todo", "/garrison/ns/todo/shares/media/../../../..", "/data/media", nil),
		"no namespace":         media("", "/garrison/ns//shares/media", "/data/media", nil),
		"a namespace with dots": media("../x", "/garrison/ns/../x/shares/media", "/data/media", nil),
		"shared propagation":   media("todo", "/garrison/ns/todo/shares/media", "/data/media", map[string]any{"Propagation": "rshared"}),
		"twice": func(c, h map[string]any) {
			media("todo", "/garrison/ns/todo/shares/media", "/data/media", nil)(c, h)
			media("todo", "/garrison/ns/todo/shares/media", "/data/media", nil)(c, h)
		},
		"a bind in Binds": func(c, h map[string]any) {
			c["Labels"] = map[string]string{"garrison.sandbox": "true", "garrison.namespace": "todo"}
			h["Binds"] = []string{"/garrison/ns/todo/shares/media:/data/media"}
		},
	} {
		if code := do(t, srv, "POST", "/v1.47/containers/create?name=garrison-agent-3", sandboxSpec(change)); code != http.StatusForbidden {
			t.Errorf("%s: %d", name, code)
		}
	}
}

// The runner's guard, which has no Share, binds nothing.
func TestAJobBindsNothing(t *testing.T) {
	srv, _ := setup(t)
	if code := do(t, srv, "POST", "/v1.47/containers/create", job(map[string]any{
		"Mounts": []map[string]any{{"Type": "bind", "Source": "/garrison/ns/todo/shares/media", "Target": "/data/media"}}})); code != http.StatusForbidden {
		t.Errorf("a job's bind: %d", code)
	}
}
