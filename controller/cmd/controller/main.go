// Command controller applies Git to the cluster. For now it offers the
// offline parts: validating stack files, checking hostname claims and
// rendering a stack for Swarm.
package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"gopkg.in/yaml.v3"

	"homecloud/controller/internal/hostnames"
	"homecloud/controller/internal/render"
	"homecloud/controller/internal/stack"
)

const usage = `usage:
  controller validate STACK.yaml...
  controller claims [-reserved FILE] STACK.yaml...
  controller render -domain DOMAIN [-env ENV] [-data-root DIR] STACK.yaml
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

	var reserved struct {
		Reserved []string `yaml:"reserved"`
	}
	if *reservedFile != "" {
		data, err := os.ReadFile(*reservedFile)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(data, &reserved); err != nil {
			return fmt.Errorf("%s: %w", *reservedFile, err)
		}
	}

	reg := hostnames.NewRegistry(reserved.Reserved)
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
