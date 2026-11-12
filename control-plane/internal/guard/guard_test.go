package guard

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
			io.WriteString(w, `{"Volumes":[{"Name":"GITEA-ACTIONS-TASK-1"},{"Name":"homecloud-buildkit"}],"Warnings":null}`)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			io.WriteString(w, `[{"Id":"job1","Names":["/GITEA-ACTIONS-TASK-1_job"]},{"Id":"gitea1","Names":["/gitea_gitea.1"]}]`)
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"Id":"job1"}`)
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
	g := New(sock, Policy{Network: "homecloud-builds", VolumePrefixes: []string{"GITEA-ACTIONS-", "act-toolcache"}})
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
	h := map[string]any{"NetworkMode": "homecloud-builds", "Binds": []string{"GITEA-ACTIONS-TASK-1:/workspace", "act-toolcache:/toolcache"}}
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
		"system network":      {"NetworkMode": "homecloud-system"},
		"host bind":           {"Binds": []string{"/:/host"}},
		"docker socket":       {"Binds": []string{"/var/run/docker.sock:/var/run/docker.sock"}},
		"foreign volume":      {"Binds": []string{"gitea-data:/data"}},
		"bind mount":          {"Mounts": []any{map[string]string{"Type": "bind", "Source": "/etc"}}},
		"volumes from":        {"VolumesFrom": []string{"gitea_gitea.1"}},
		"other runtime":       {"Runtime": "sysbox-runc"},
		"sysctl":              {"Sysctls": map[string]string{"kernel.core_pattern": "|/x"}},
		"published port":      {"PortBindings": map[string]any{"80/tcp": []any{}}},
		"cgroup parent":       {"CgroupParent": "/"},
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
		{"POST", "/v1.47/images/postgres/tag?repo=git.example.com/system/control-plane", nil},
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
		{"DELETE", "/v1.47/volumes/homecloud-data", nil},
		{"POST", "/v1.47/networks/create", map[string]any{"Name": "x"}},
	} {
		if got := do(t, srv, c.method, c.path, c.body); got != 403 {
			t.Errorf("%s %s: %d, want 403", c.method, c.path, got)
		}
	}
	if len(*seen) != before {
		t.Errorf("refused requests reached Docker: %v", (*seen)[before:])
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
