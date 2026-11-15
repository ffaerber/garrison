// Command homecloud applies Git to the cluster. For now it offers the
// offline parts: validating stack files, checking hostname claims and
// rendering a stack for Swarm.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	_ "time/tzdata" // schedules in the home cloud's time zone, whatever the image holds

	"gopkg.in/yaml.v3"

	"github.com/coreos/go-oidc/v3/oidc"

	"homecloud/control-plane/internal/agentapi"
	"homecloud/control-plane/internal/apply"
	"homecloud/control-plane/internal/dashboards"
	"homecloud/control-plane/internal/events"
	"homecloud/control-plane/internal/gateway"
	"homecloud/control-plane/internal/gitops"
	"homecloud/control-plane/internal/guard"
	"homecloud/control-plane/internal/health"
	"homecloud/control-plane/internal/home"
	"homecloud/control-plane/internal/hooks"
	"homecloud/control-plane/internal/hostnames"
	"homecloud/control-plane/internal/render"
	"homecloud/control-plane/internal/sandbox"
	"homecloud/control-plane/internal/stack"
)

const usage = `usage:
  homecloud validate STACK.yaml...
  homecloud validate --in NAMESPACE NAME.yaml...   (as config/stacks/NAME.yaml of NAMESPACE)
  homecloud claims [-reserved FILE] STACK.yaml...
  homecloud render -domain DOMAIN [-env ENV] [-data-root DIR] STACK.yaml
  homecloud apply  -domain DOMAIN [-env ENV] [-data-root DIR] [-state FILE] [-reserved FILE] STACK.yaml
  homecloud deploy [-once] [-interval DURATION] [-config FILE] [-work DIR]
                   [-gitea URL] [-password-file FILE] [-host-exec IMAGE]
  homecloud applied [-work DIR]
  homecloud gateway [-listen ADDR] [-gitea URL] [-openwebui URL] [-prometheus URL] ...
  homecloud agents [-listen ADDR] [-gitea URL] [-gateway URL] ...
  homecloud home [-listen ADDR] [-gitea URL] [-events-db URL] ...
  homecloud guard [-listen ADDR] [-socket PATH] [-network NAME] [-volumes PREFIXES]
                  [-image REPOSITORY -label LABEL -max-memory BYTES]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "validate":
		err = validate(args)
	case "claims":
		err = claims(args)
	case "render":
		err = renderCmd(args)
	case "apply":
		err = applyCmd(args)
	case "deploy":
		err = deployCmd(args)
	case "applied":
		err = appliedCmd(args)
	case "gateway":
		err = gatewayCmd(args)
	case "agents":
		err = agentsCmd(args)
	case "guard":
		err = guardCmd(args)
	case "home":
		err = homeCmd(args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func load(file string) (*stack.Stack, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	s, err := stack.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return s, nil
}

// validate checks stack files: standalone ones, or with --in NAMESPACE
// ones as stacks/<name>.yaml of that namespace's config repo.
func validate(files []string) error {
	ns := ""
	if len(files) >= 2 && files[0] == "--in" {
		ns, files = files[1], files[2:]
	}
	if len(files) == 0 {
		return fmt.Errorf("validate needs at least one stack file")
	}
	for _, f := range files {
		var err error
		if ns == "" {
			_, err = load(f)
		} else {
			var data []byte
			if data, err = os.ReadFile(f); err == nil {
				_, err = stack.ParseIn(data, ns, strings.TrimSuffix(filepath.Base(f), ".yaml"))
			}
			if err != nil {
				err = fmt.Errorf("%s: %w", f, err)
			}
		}
		if err != nil {
			return err
		}
		fmt.Println(f, "ok")
	}
	return nil
}

func claims(args []string) error {
	fs := flag.NewFlagSet("claims", flag.ExitOnError)
	reservedFile := fs.String("reserved", "", "YAML file with a `reserved` list of hostnames")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return fmt.Errorf("claims needs at least one stack file")
	}

	reserved, err := loadReserved(*reservedFile)
	if err != nil {
		return err
	}
	reg := hostnames.NewRegistry(reserved)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOSTNAME\tNAMESPACE\tSTACK\tENVIRONMENT\tSERVICE")
	for _, f := range fs.Args() {
		s, err := load(f)
		if err != nil {
			return err
		}
		if err := reg.Claim(s); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		for _, c := range hostnames.Of(s) {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.Hostname, c.Namespace, c.Stack, c.Environment, c.Service)
		}
	}
	return w.Flush()
}

func loadReserved(file string) ([]string, error) {
	if file == "" {
		return nil, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Reserved []string `yaml:"reserved"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return doc.Reserved, nil
}

// applyCmd claims the stack's hostnames, then deploys one environment to the
// local Swarm. The claims are saved only after a successful deploy.
func applyCmd(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	var o render.Options
	fs.StringVar(&o.Environment, "env", stack.MainEnvironment, "environment to deploy")
	fs.StringVar(&o.Domain, "domain", "", "installation domain, e.g. example.com")
	fs.StringVar(&o.DataRoot, "data-root", "", "dataset mount that holds all namespaces (default "+render.DefaultDataRoot+")")
	state := fs.String("state", "/var/lib/homecloud/control-plane/claims.json", "file that records held hostnames; shared with the deployer")
	reservedFile := fs.String("reserved", "", "YAML file with a `reserved` list of hostnames")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("apply needs exactly one stack file")
	}
	s, err := load(fs.Arg(0))
	if err != nil {
		return err
	}
	reserved, err := loadReserved(*reservedFile)
	if err != nil {
		return err
	}
	reg := hostnames.NewRegistry(reserved)
	if err := reg.Load(*state); err != nil {
		return err
	}
	if err := reg.Claim(s); err != nil {
		return err
	}
	if err := apply.Stack(context.Background(), apply.Exec, s, o); err != nil {
		return err
	}
	if err := reg.Save(*state); err != nil {
		return err
	}
	fmt.Printf("deployed %s/%s (%s) as https://%s.%s\n", s.Namespace, s.Name, o.Environment, s.Hostname(o.Environment), o.Domain)
	return nil
}

const (
	etcDir  = "/etc/homecloud"
	workDir = "/var/lib/homecloud/deployer"
)

// readEnvFile reads KEY=VALUE lines, as the installer writes install.env.
func readEnvFile(file string) ([]string, map[string]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	var env []string
	vars := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, nil, fmt.Errorf("%s: not KEY=VALUE: %q", file, line)
		}
		env = append(env, line)
		vars[k] = v
	}
	return env, vars, nil
}

// deployCmd follows the `system` organisation in Gitea and deploys every repo
// in it that holds a compose.yaml. With -once it deploys them all a single time.
func deployCmd(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	once := fs.Bool("once", false, "deploy every stack once and exit")
	interval := fs.Duration("interval", 15*time.Second, "how often to check the repos")
	config := fs.String("config", etcDir+"/install.env", "installation settings")
	work := fs.String("work", workDir, "the deployer's checkout and state")
	gitea := fs.String("gitea", "", "Gitea's URL (default https://git.<domain>, through Traefik on this machine)")
	passwordFile := fs.String("password-file", etcDir+"/admin-password", "password of the Gitea admin account")
	hostExec := fs.String("host-exec", "", "run ZFS on the host through a privileged container of this image (for a deployer in a container)")
	grafana := fs.String("grafana", "", "Grafana's URL; when set, dashboards/*.json in the repos are kept in Grafana")
	grafanaUser := fs.String("grafana-user", "installer", "Grafana's local admin, whose password is the admin password")
	apps := fs.Bool("apps", true, "also deploy the apps of every other namespace (repos with a stack.yaml)")
	listen := fs.String("listen", "", "address to take Gitea's webhook on (/hook); empty: only check every interval")
	hookURL := fs.String("hook-url", "", "the URL Gitea reaches the webhook at")
	dockerSocket := fs.String("docker-socket", "/var/run/docker.sock", "Docker's API, for the services' metrics on -listen (/metrics)")
	feed := eventsFlags(fs)
	fs.Parse(args)

	env, vars, err := readEnvFile(*config)
	if err != nil {
		return err
	}
	if vars["DOMAIN"] == "" || vars["ADMIN_USER"] == "" {
		return fmt.Errorf("%s must set DOMAIN and ADMIN_USER", *config)
	}
	c := &gitops.Config{
		Domain:       vars["DOMAIN"],
		GiteaURL:     *gitea,
		User:         vars["ADMIN_USER"],
		PasswordFile: *passwordFile,
		CAFile:       etcDir + "/tls.crt",
		Org:          "system",
		Apps:         *apps,
		WorkDir:      *work,
		Env:          env,
		Run:          apply.Exec,
	}
	if *hostExec != "" {
		c.Run = apply.HostExec(*hostExec)
	}
	store, err := feed.store()
	if err != nil {
		return err
	}
	var onEvent func(event, delivery string, body []byte)
	if store != nil {
		c.Events = store
		// Gitea's deliveries go to the feed too, in the background: Gitea
		// waits for the answer.
		onEvent = func(event, delivery string, body []byte) {
			go func() {
				for _, e := range events.FromGitea(event, delivery, body) {
					store.Record(context.Background(), e)
				}
			}()
		}
	}
	if *grafana != "" {
		pw, err := os.ReadFile(*passwordFile)
		if err != nil {
			return err
		}
		c.Dashboards = &dashboards.Grafana{URL: *grafana, User: *grafanaUser, Password: strings.TrimSpace(string(pw))}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		s, err := c.Once(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("applied %d stacks\n", len(s.Stacks))
		return nil
	}
	if *listen != "" && *hookURL != "" {
		pw, err := os.ReadFile(*passwordFile)
		if err != nil {
			return err
		}
		giteaURL := c.GiteaURL
		if giteaURL == "" {
			giteaURL = "https://git." + c.Domain
		}
		wake := &hooks.Wake{}
		c.Wake = wake.Subscribe()
		if err := serveHook(ctx, *listen, *hookURL, wake, &hooks.Gitea{URL: giteaURL, User: c.User, Password: strings.TrimSpace(string(pw))}, onEvent, (&health.Docker{Socket: *dockerSocket}).Handler()); err != nil {
			return err
		}
	}
	log.Printf("following the %s organisation every %s", c.Org, *interval)
	return c.Serve(ctx, *interval)
}

// appliedCmd prints each applied stack and the commit of its repo.
func appliedCmd(args []string) error {
	fs := flag.NewFlagSet("applied", flag.ExitOnError)
	work := fs.String("work", workDir, "the deployer's checkouts and state")
	fs.Parse(args)
	s, err := gitops.ReadState(*work)
	if err != nil {
		return err
	}
	if len(s.Stacks) == 0 {
		return fmt.Errorf("nothing applied yet")
	}
	for _, name := range s.Names() {
		fmt.Println(name, s.Stacks[name])
	}
	keys := make([]string, 0, len(s.Apps))
	for k := range s.Apps {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		a := s.Apps[k]
		fmt.Printf("app %s %s %s\n", k, a.Stack, a.Commit)
	}
	return nil
}

// gatewayCmd serves the gateway: the MCP server Open WebUI's
// agents use, and the sync that keeps every namespace's agents there.
func gatewayCmd(args []string) error {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "address to serve MCP on (/mcp, /mcp/<namespace>/<agent>)")
	config := fs.String("config", etcDir+"/install.env", "installation settings")
	work := fs.String("work", workDir, "the deployer's state, read-only")
	gitea := fs.String("gitea", "http://gitea_gitea:3000", "Gitea's URL")
	passwordFile := fs.String("password-file", etcDir+"/admin-password", "password of the Gitea and Open WebUI admin accounts")
	caFile := fs.String("ca", "/run/secrets/tls.crt", "certificate the installation's TLS is verified with")
	prometheus := fs.String("prometheus", "http://prometheus_prometheus:9090", "Prometheus's URL")
	openwebui := fs.String("openwebui", "http://openwebui_openwebui:8080", "Open WebUI's URL")
	browser := fs.String("browser", "http://control-plane_browser:9222", "headless Chrome's DevTools, for screenshots; empty: none")
	clients := fs.String("clients", "openwebui", "Keycloak clients whose access tokens are accepted, comma-separated")
	hookURL := fs.String("hook-url", "", "the URL Gitea reaches this server's webhook (/hook) at; empty: only check every interval")
	feed := eventsFlags(fs)
	fs.Parse(args)

	_, vars, err := readEnvFile(*config)
	if err != nil {
		return err
	}
	store, err := feed.store()
	if err != nil {
		return err
	}
	domain := vars["DOMAIN"]
	if domain == "" || vars["ADMIN_USER"] == "" {
		return fmt.Errorf("%s must set DOMAIN and ADMIN_USER", *config)
	}
	password, err := os.ReadFile(*passwordFile)
	if err != nil {
		return err
	}
	pem, err := os.ReadFile(*caFile)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	verifier, err := keycloakVerifier(ctx, domain, pem, *caFile, strings.Split(*clients, ","))
	if err != nil {
		return err
	}

	g := &gateway.Gitea{URL: *gitea, User: vars["ADMIN_USER"], Password: strings.TrimSpace(string(password))}
	catalog := &gateway.Catalog{Gitea: g}
	tools := &gateway.Tools{Gitea: g, Run: apply.Exec, WorkDir: *work, Prometheus: *prometheus}
	if *browser != "" {
		tools.Browser = &gateway.Browser{URL: *browser, Domain: domain, Private: tools.PrivateHost}
	}
	serviceKey := gateway.ServiceKey(g.Password)
	mcp := &gateway.MCP{
		Tools:      tools,
		Verifier:   verifier,
		Agents:     catalog,
		ServiceKey: serviceKey,
	}
	if store != nil {
		mcp.Events = store
	}
	sync := &gateway.OpenWebUI{
		URL: *openwebui, Email: "installer@" + domain, Password: strings.TrimSpace(string(password)),
		Domain: domain, Gitea: g, Agents: catalog,
	}
	// Gitea's webhook wakes the syncs; the intervals stay for missed deliveries.
	wake := &hooks.Wake{}
	sync.Wake = wake.Subscribe()
	go sync.Run(ctx, time.Minute)
	ca := ""
	if vars["TLS_MODE"] == "selfsigned" {
		ca = string(pem)
	}
	go (&gateway.Builders{Gitea: g, Domain: domain, CA: ca, Wake: wake.Subscribe()}).Run(ctx, time.Minute)
	go (&gateway.Members{Gitea: g, Wake: wake.Subscribe()}).Run(ctx, 20*time.Second)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp)
	mux.Handle("/mcp/", mcp)
	mux.Handle("POST /internal/post", gateway.PostHandler(sync, serviceKey))
	mux.Handle("GET /internal/channels/{id}", gateway.ChannelHandler(sync, serviceKey))
	if *hookURL != "" {
		secret := hooks.Secret(g.Password, *hookURL)
		mux.Handle("POST /hook", &hooks.Handler{Secret: secret, Wake: wake})
		go (&hooks.Gitea{URL: *gitea, User: g.User, Password: g.Password}).Keep(ctx, *hookURL, secret, 10*time.Second)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("gateway: MCP on %s/mcp", *listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// serveHook takes Gitea's webhook on listen and registers it with Gitea as
// target. It returns once listening.
func serveHook(ctx context.Context, listen, target string, wake *hooks.Wake, g *hooks.Gitea, onEvent func(event, delivery string, body []byte), metrics http.Handler) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	secret := hooks.Secret(g.Password, target)
	mux := http.NewServeMux()
	mux.Handle("POST /hook", &hooks.Handler{Secret: secret, Wake: wake, OnEvent: onEvent})
	if metrics != nil {
		mux.Handle("GET /metrics", metrics)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go server.Serve(ln)
	go func() { <-ctx.Done(); server.Close() }()
	go g.Keep(ctx, target, secret, 10*time.Second)
	return nil
}

// guardCmd serves the Docker API to the user namespaces' Actions runner,
// letting through only what running jobs needs; with -image and -label, to
// the agent service, letting through only sandboxes.
func guardCmd(args []string) error {
	fs := flag.NewFlagSet("guard", flag.ExitOnError)
	listen := fs.String("listen", ":2375", "address to serve the guarded Docker API on")
	socket := fs.String("socket", "/var/run/docker.sock", "the Docker daemon's socket")
	network := fs.String("network", "homecloud-builds", "the only network containers may join")
	volumes := fs.String("volumes", "GITEA-ACTIONS-,act-toolcache", "prefixes of the volumes containers may use, comma-separated")
	image := fs.String("image", "", "the only image repository containers may run, if set")
	label := fs.String("label", "", "the label containers must carry, and are known by, if set")
	maxMemory := fs.Int64("max-memory", 0, "the largest memory limit a container may ask for; if set, it must ask for one")
	runtime := fs.String("runtime", "", "the runtime containers must run under, if set, e.g. runsc (gVisor)")
	fs.Parse(args)
	g := guard.New(*socket, guard.Policy{Network: *network, VolumePrefixes: strings.Split(*volumes, ","),
		Image: *image, Label: *label, MaxMemory: *maxMemory, Runtime: *runtime})
	log.Printf("docker guard on %s for %s, containers on %s", *listen, *socket, *network)
	return (&http.Server{Addr: *listen, Handler: g, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe()
}

func renderCmd(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	var o render.Options
	fs.StringVar(&o.Environment, "env", stack.MainEnvironment, "environment to render")
	fs.StringVar(&o.Domain, "domain", "", "installation domain, e.g. example.com")
	fs.StringVar(&o.DataRoot, "data-root", "", "where the ZFS pool is mounted (default /homecloud)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("render needs exactly one stack file")
	}
	s, err := load(fs.Arg(0))
	if err != nil {
		return err
	}
	out, err := render.Swarm(s, o)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(out)
	return err
}

// keycloakVerifier accepts access tokens of the installation's Keycloak
// realm issued to clients. Keycloak may still be starting.
func keycloakVerifier(ctx context.Context, domain string, pem []byte, caFile string, clients []string) (*gateway.KeycloakVerifier, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no certificate", caFile)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	issuer := "https://keycloak." + domain + "/realms/homecloud"
	for {
		verifier, err := gateway.NewKeycloakVerifier(oidc.ClientContext(ctx, httpClient), issuer, clients)
		if err == nil {
			return verifier, nil
		}
		log.Printf("waiting for Keycloak: %v", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// agentsCmd serves the agent service: the OpenAI-compatible API Open WebUI
// talks to, whose models are the namespaces' agents. It runs each turn of an
// agent: the LLM, which only it has the key of, with the agent's tools from
// the gateway, acting as the person chatting.
func agentsCmd(args []string) error {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	listen := fs.String("listen", ":8070", "address to serve the API on (/v1)")
	config := fs.String("config", etcDir+"/install.env", "installation settings")
	giteaURL := fs.String("gitea", "http://gitea_gitea:3000", "Gitea's URL")
	gatewayURL := fs.String("gateway", "http://control-plane_gateway:8080/mcp", "the gateway's MCP endpoint")
	passwordFile := fs.String("password-file", etcDir+"/admin-password", "password of the Gitea admin account")
	keyFile := fs.String("llm-key-file", etcDir+"/llm-api-key", "the LLM's API key")
	caFile := fs.String("ca", "/run/secrets/tls.crt", "certificate the installation's TLS is verified with")
	clients := fs.String("clients", "openwebui", "Keycloak clients whose access tokens are accepted, comma-separated")
	image := fs.String("sandbox-image", "", "the sandbox image; if empty, turns run in this process")
	dockerAddr := fs.String("sandbox-docker", "control-plane_sandbox-guard:2375", "the Docker API sandboxes are started through")
	network := fs.String("sandbox-network", "homecloud-sandboxes", "the network of the sandboxes")
	sandboxListen := fs.String("sandbox-listen", ":8071", "address to serve the sandboxes on: the LLM and the tools")
	sandboxAPI := fs.String("sandbox-api", "http://control-plane_agents:8071", "that address, as sandboxes reach it")
	memory := fs.Int64("sandbox-memory", 1<<30, "memory limit of a sandbox, in bytes")
	cpus := fs.Float64("sandbox-cpus", 1, "CPU limit of a sandbox")
	idle := fs.Duration("sandbox-idle", 15*time.Minute, "a sandbox without a message this long is stopped")
	perPerson := fs.Int("sandbox-per-person", 3, "running sandboxes per person")
	runtime := fs.String("sandbox-runtime", "runsc", "the runtime sandboxes run under (gVisor), as root; empty: Docker's default, as the image's user")
	internetListen := fs.String("internet-listen", ":8072", "address to serve the sandboxes' proxy to the internet on")
	internetProxy := fs.String("internet-proxy", "http://control-plane_agents:8072", "that address, as sandboxes reach it; empty: no internet for any agent")
	alertsListen := fs.String("alerts-listen", "", "address to take Alertmanager's webhook on (/alerts), which starts routines with on_alert; empty: none")
	alertsTokenFile := fs.String("alerts-token-file", "/run/secrets/alerts-token", "the bearer token Alertmanager sends")
	feed := eventsFlags(fs)
	fs.Parse(args)
	store, err := feed.store()
	if err != nil {
		return err
	}

	_, vars, err := readEnvFile(*config)
	if err != nil {
		return err
	}
	domain := vars["DOMAIN"]
	if domain == "" || vars["ADMIN_USER"] == "" {
		return fmt.Errorf("%s must set DOMAIN and ADMIN_USER", *config)
	}
	model := vars["LLM_MODEL"]
	if model == "" {
		model = "gpt-oss:120b"
	}
	base := vars["LLM_BASE_URL"]
	if base == "" {
		base = "https://ollama.com/v1"
	}
	password, err := os.ReadFile(*passwordFile)
	if err != nil {
		return err
	}
	pem, err := os.ReadFile(*caFile)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	verifier, err := keycloakVerifier(ctx, domain, pem, *caFile, strings.Split(*clients, ","))
	if err != nil {
		return err
	}
	g := &gateway.Gitea{URL: *giteaURL, User: vars["ADMIN_USER"], Password: strings.TrimSpace(string(password))}
	srv := &agentapi.Server{
		Agents:       &agentapi.GiteaAgents{Gitea: g},
		Verifier:     verifier,
		LLM:          &agentapi.LLM{URL: base, KeyFile: *keyFile},
		Gateway:      &agentapi.Gateway{URL: *gatewayURL, ServiceKey: gateway.ServiceKey(g.Password)},
		Domain:       domain,
		DefaultModel: model,
		Gitea:        g,
		Poster:       &agentapi.GatewayPoster{URL: strings.TrimSuffix(*gatewayURL, "/mcp"), ServiceKey: gateway.ServiceKey(g.Password)},
		Events:       recorder(store),
	}
	if *image != "" {
		// The sandbox image is pulled from Gitea's registry as the admin, if
		// the build did not leave it on this machine.
		auth, _ := json.Marshal(map[string]string{"username": vars["ADMIN_USER"], "password": strings.TrimSpace(string(password)), "serveraddress": "git." + domain})
		srv.Sandboxes = &sandbox.Manager{
			Docker: &sandbox.Docker{Addr: *dockerAddr}, Image: *image, Network: *network, API: *sandboxAPI,
			Proxy: *internetProxy, Runtime: *runtime,
			Memory: *memory, CPUs: *cpus, Pids: 512, Idle: *idle, PerPerson: *perPerson,
			RegistryAuth: base64.URLEncoding.EncodeToString(auth),
		}
		go func() {
			for ctx.Err() == nil {
				err := srv.Sandboxes.Clean(ctx)
				if err == nil {
					break
				}
				log.Printf("agents: sandboxes: %v", err)
				time.Sleep(5 * time.Second)
			}
			srv.Sandboxes.Run(ctx)
		}()
		srv.Git = &agentapi.GitProxy{Gitea: g, Agents: srv.Agents}
		sb := &http.Server{Addr: *sandboxListen, Handler: srv.SandboxHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			sb.Close()
		}()
		if *internetProxy != "" {
			proxy := &http.Server{Addr: *internetListen, Handler: &agentapi.InternetProxy{Server: srv}, ReadHeaderTimeout: 10 * time.Second}
			go func() {
				<-ctx.Done()
				proxy.Close()
			}()
			go func() {
				if err := proxy.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Fatalf("agents: internet proxy: %v", err)
				}
			}()
		}
		go func() {
			if err := sb.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("agents: sandboxes: %v", err)
			}
		}()
		routines := &agentapi.Routines{Server: srv, Gitea: g, Location: zone(vars["TIMEZONE"])}
		if store != nil {
			routines.Store = store
		}
		go routines.Run(ctx)
		if *alertsListen != "" {
			token, err := os.ReadFile(*alertsTokenFile)
			if err != nil {
				return fmt.Errorf("-alerts-token-file: %w", err)
			}
			mux := http.NewServeMux()
			mux.Handle("POST /alerts", routines.AlertHandler(strings.TrimSpace(string(token))))
			alerts := &http.Server{Addr: *alertsListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			go func() {
				if err := alerts.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Fatalf("agents: alerts: %v", err)
				}
			}()
			log.Printf("agents: Alertmanager's webhook on %s/alerts", *alertsListen)
		}
		log.Printf("agents: sandboxes from %s on %s under %q, served on %s, proxy on %s", *image, *network, *runtime, *sandboxListen, *internetListen)
	}
	// Turns stream for minutes: no write timeout.
	server := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("agents: API on %s/v1, LLM %s at %s", *listen, model, base)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// feedFlags name the feed's database.
type feedFlags struct{ url, passwordFile *string }

func eventsFlags(fs *flag.FlagSet) feedFlags {
	return feedFlags{
		url:          fs.String("events-db", "", "the feed's Postgres database, e.g. postgres://homecloud@postgres_postgres:5432/homecloud; empty: no feed"),
		passwordFile: fs.String("events-db-password-file", "/run/secrets/homecloud-db-password", "its password"),
	}
}

// store is the feed's store, or nil without -events-db. It connects when
// first used: Postgres may not answer yet.
func (f feedFlags) store() (*events.Lazy, error) {
	if *f.url == "" {
		return nil, nil
	}
	u, err := url.Parse(*f.url)
	if err != nil || u.User == nil {
		return nil, fmt.Errorf("-events-db: want postgres://USER@HOST/DATABASE, got %q", *f.url)
	}
	pw, err := os.ReadFile(*f.passwordFile)
	if err != nil {
		return nil, err
	}
	u.User = url.UserPassword(u.User.Username(), strings.TrimSpace(string(pw)))
	q := u.Query()
	if q.Get("sslmode") == "" {
		q.Set("sslmode", "disable") // on the system network only
	}
	u.RawQuery = q.Encode()
	return &events.Lazy{URL: u.String()}, nil
}

// recorder avoids a non-nil interface holding a nil store.
func recorder(s *events.Lazy) events.Recorder {
	if s == nil {
		return nil
	}
	return s
}

// homeCmd serves the dashboard: the feed of what happens and how it was
// handled, to each person what they may read. Traefik puts it behind the
// sign-in, which names the person.
func homeCmd(args []string) error {
	fs := flag.NewFlagSet("home", flag.ExitOnError)
	listen := fs.String("listen", ":8060", "address to serve the dashboard on")
	config := fs.String("config", etcDir+"/install.env", "installation settings")
	gitea := fs.String("gitea", "http://gitea_gitea:3000", "Gitea's URL, to ask what each person may read")
	passwordFile := fs.String("password-file", etcDir+"/admin-password", "password of the Gitea admin account")
	feed := eventsFlags(fs)
	fs.Parse(args)

	_, vars, err := readEnvFile(*config)
	if err != nil {
		return err
	}
	if vars["DOMAIN"] == "" || vars["ADMIN_USER"] == "" {
		return fmt.Errorf("%s must set DOMAIN and ADMIN_USER", *config)
	}
	password, err := os.ReadFile(*passwordFile)
	if err != nil {
		return err
	}
	store, err := feed.store()
	if err != nil {
		return err
	}
	if store == nil {
		return fmt.Errorf("-events-db is needed")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go store.Prune(ctx)
	g := &gateway.Gitea{URL: *gitea, User: vars["ADMIN_USER"], Password: strings.TrimSpace(string(password))}
	s := &home.Server{Events: store, Access: g, Domain: vars["DOMAIN"]}
	server := &http.Server{Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("home: the dashboard on %s", *listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// zone is the home cloud's time zone, TIMEZONE in install.env (e.g.
// Europe/Berlin); UTC if unset or unknown.
func zone(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		log.Printf("TIMEZONE %q: %v; schedules run in UTC", name, err)
		return time.UTC
	}
	return loc
}
