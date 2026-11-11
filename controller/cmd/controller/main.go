// Command controller applies Git to the cluster. For now it offers the
// offline parts: validating stack files, checking hostname claims and
// rendering a stack for Swarm.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/coreos/go-oidc/v3/oidc"

	"homecloud/controller/internal/apply"
	"homecloud/controller/internal/controlplane"
	"homecloud/controller/internal/dashboards"
	"homecloud/controller/internal/gitops"
	"homecloud/controller/internal/guard"
	"homecloud/controller/internal/hostnames"
	"homecloud/controller/internal/render"
	"homecloud/controller/internal/stack"
)

const usage = `usage:
  controller validate STACK.yaml...
  controller claims [-reserved FILE] STACK.yaml...
  controller render -domain DOMAIN [-env ENV] [-data-root DIR] STACK.yaml
  controller apply  -domain DOMAIN [-env ENV] [-data-root DIR] [-state FILE] [-reserved FILE] STACK.yaml
  controller serve [-once] [-interval DURATION] [-config FILE] [-work DIR]
                   [-gitea URL] [-password-file FILE] [-host-exec IMAGE]
  controller applied [-work DIR]
  controller controlplane [-listen ADDR] [-gitea URL] [-openwebui URL] [-prometheus URL] ...
  controller docker-guard [-listen ADDR] [-socket PATH] [-network NAME]
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
	case "serve":
		err = serveCmd(args)
	case "applied":
		err = appliedCmd(args)
	case "controlplane":
		err = controlplaneCmd(args)
	case "docker-guard":
		err = guardCmd(args)
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

func validate(files []string) error {
	if len(files) == 0 {
		return fmt.Errorf("validate needs at least one stack file")
	}
	for _, f := range files {
		if _, err := load(f); err != nil {
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
	state := fs.String("state", "/var/lib/homecloud/controller/claims.json", "file that records held hostnames; shared with the controller")
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
	workDir = "/var/lib/homecloud/controller"
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

// serveCmd follows the `system` organisation in Gitea and deploys every repo
// in it that holds a compose.yaml. With -once it deploys them all a single time.
func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	once := fs.Bool("once", false, "deploy every stack once and exit")
	interval := fs.Duration("interval", 15*time.Second, "how often to check the repos")
	config := fs.String("config", etcDir+"/install.env", "installation settings")
	work := fs.String("work", workDir, "the controller's checkout and state")
	gitea := fs.String("gitea", "", "Gitea's URL (default https://git.<domain>, through Traefik on this machine)")
	passwordFile := fs.String("password-file", etcDir+"/admin-password", "password of the Gitea admin account")
	hostExec := fs.String("host-exec", "", "run ZFS on the host through a privileged container of this image (for a controller in a container)")
	grafana := fs.String("grafana", "", "Grafana's URL; when set, dashboards/*.json in the repos are kept in Grafana")
	grafanaUser := fs.String("grafana-user", "installer", "Grafana's local admin, whose password is the admin password")
	apps := fs.Bool("apps", true, "also deploy the apps of every other namespace (repos with a stack.yaml)")
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
	log.Printf("following the %s organisation every %s", c.Org, *interval)
	return c.Serve(ctx, *interval)
}

// appliedCmd prints each applied stack and the commit of its repo.
func appliedCmd(args []string) error {
	fs := flag.NewFlagSet("applied", flag.ExitOnError)
	work := fs.String("work", workDir, "the controller's checkouts and state")
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

// controlplaneCmd serves the control plane: the MCP server Open WebUI's
// agents use, and the sync that keeps one agent per namespace there.
func controlplaneCmd(args []string) error {
	fs := flag.NewFlagSet("controlplane", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "address to serve MCP on (/mcp)")
	config := fs.String("config", etcDir+"/install.env", "installation settings")
	work := fs.String("work", workDir, "the controller's state, read-only")
	gitea := fs.String("gitea", "http://gitea_gitea:3000", "Gitea's URL")
	passwordFile := fs.String("password-file", etcDir+"/admin-password", "password of the Gitea and Open WebUI admin accounts")
	caFile := fs.String("ca", "/run/secrets/tls.crt", "certificate the installation's TLS is verified with")
	prometheus := fs.String("prometheus", "http://prometheus_prometheus:9090", "Prometheus's URL")
	openwebui := fs.String("openwebui", "http://openwebui_openwebui:8080", "Open WebUI's URL")
	clients := fs.String("clients", "openwebui", "Keycloak clients whose access tokens are accepted, comma-separated")
	fs.Parse(args)

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
	password, err := os.ReadFile(*passwordFile)
	if err != nil {
		return err
	}
	pem, err := os.ReadFile(*caFile)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return fmt.Errorf("%s: no certificate", *caFile)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Keycloak may still be starting.
	var verifier *controlplane.KeycloakVerifier
	issuer := "https://keycloak." + domain + "/realms/homecloud"
	for {
		verifier, err = controlplane.NewKeycloakVerifier(oidc.ClientContext(ctx, httpClient), issuer, strings.Split(*clients, ","))
		if err == nil {
			break
		}
		log.Printf("waiting for Keycloak: %v", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}

	g := &controlplane.Gitea{URL: *gitea, User: vars["ADMIN_USER"], Password: strings.TrimSpace(string(password))}
	mcp := &controlplane.MCP{
		Tools:    &controlplane.Tools{Gitea: g, Run: apply.Exec, WorkDir: *work, Prometheus: *prometheus},
		Verifier: verifier,
	}
	sync := &controlplane.OpenWebUI{
		URL: *openwebui, Email: "installer@" + domain, Password: strings.TrimSpace(string(password)),
		BaseModel: model, ToolServerID: "homecloud", Domain: domain, Gitea: g,
	}
	go sync.Run(ctx, time.Minute)
	ca := ""
	if vars["TLS_MODE"] == "selfsigned" {
		ca = string(pem)
	}
	go (&controlplane.Builders{Gitea: g, Domain: domain, CA: ca}).Run(ctx, time.Minute)
	go (&controlplane.Members{Gitea: g}).Run(ctx, 20*time.Second)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("control plane: MCP on %s/mcp, agents on %s", *listen, model)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// guardCmd serves the Docker API to the user namespaces' Actions runner,
// letting through only what running jobs needs.
func guardCmd(args []string) error {
	fs := flag.NewFlagSet("docker-guard", flag.ExitOnError)
	listen := fs.String("listen", ":2375", "address to serve the guarded Docker API on")
	socket := fs.String("socket", "/var/run/docker.sock", "the Docker daemon's socket")
	network := fs.String("network", "homecloud-builds", "the only network job containers may join")
	fs.Parse(args)
	g := guard.New(*socket, guard.Policy{Network: *network, VolumePrefixes: []string{"GITEA-ACTIONS-", "act-toolcache"}})
	log.Printf("docker guard on %s for %s, jobs on %s", *listen, *socket, *network)
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
