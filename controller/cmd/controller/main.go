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
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"homecloud/controller/internal/apply"
	"homecloud/controller/internal/controlplane"
	"homecloud/controller/internal/gitops"
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
  controller controlplane [-listen ADDR] [-gitea URL] [-password-file FILE] [-work DIR] ...
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
	state := fs.String("state", "/var/lib/homecloud/claims.json", "file that records held hostnames")
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
		WorkDir:      *work,
		Env:          env,
		Run:          apply.Exec,
	}
	if *hostExec != "" {
		c.Run = apply.HostExec(*hostExec)
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
	return nil
}

// controlplaneCmd serves the web control plane: Keycloak sign-in, stacks,
// changes waiting for approval, and the operator agent.
func controlplaneCmd(args []string) error {
	fs := flag.NewFlagSet("controlplane", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "address to serve on")
	config := fs.String("config", etcDir+"/install.env", "installation settings")
	work := fs.String("work", workDir, "the controller's state, read-only")
	gitea := fs.String("gitea", "http://gitea_gitea:3000", "Gitea's URL")
	passwordFile := fs.String("password-file", etcDir+"/admin-password", "password of the Gitea admin account")
	clientSecretFile := fs.String("client-secret-file", "/run/secrets/controlplane-oidc-secret", "Keycloak client secret")
	caFile := fs.String("ca", "/run/secrets/tls.crt", "certificate the installation's TLS is verified with")
	keyFile := fs.String("llm-key-file", etcDir+"/llm-api-key", "API key of the LLM")
	fs.Parse(args)

	_, vars, err := readEnvFile(*config)
	if err != nil {
		return err
	}
	domain := vars["DOMAIN"]
	if domain == "" || vars["ADMIN_USER"] == "" {
		return fmt.Errorf("%s must set DOMAIN and ADMIN_USER", *config)
	}
	or := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	password, err := os.ReadFile(*passwordFile)
	if err != nil {
		return err
	}
	secret, err := os.ReadFile(*clientSecretFile)
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

	g := &controlplane.Gitea{URL: *gitea, User: vars["ADMIN_USER"], Password: strings.TrimSpace(string(password)), Org: "system"}
	agent := &controlplane.Agent{
		LLM: &controlplane.LLM{
			BaseURL: or(vars["LLM_BASE_URL"], "https://ollama.com/v1"),
			Model:   or(vars["LLM_MODEL"], "gpt-oss:120b"),
			KeyFile: *keyFile,
		},
		Gitea: g, Run: apply.Exec, WorkDir: *work, Domain: domain,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv, err := controlplane.New(ctx, controlplane.Config{
		PublicURL:    "https://controlplane." + domain,
		Issuer:       "https://keycloak." + domain + "/realms/homecloud",
		ClientID:     "controlplane",
		ClientSecret: strings.TrimSpace(string(secret)),
		HTTP:         httpClient,
		Agent:        agent,
		Gitea:        g,
	})
	if err != nil {
		return err
	}
	server := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("control plane on %s, model %s", *listen, agent.LLM.(*controlplane.LLM).Model)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
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
