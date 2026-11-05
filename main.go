// Command tofu-api exposes resource types of an OpenTofu-protocol
// provider as an HTTP API, without writing code per resource: it reads each
// type's schema from the provider, generates the OpenAPI contract from it,
// and serves create/read/update/delete through pkg/tofu. One configuration
// file per provider says which, and how (see configFile).
//
//	tofu-api serve -config <provider>.yaml
//	tofu-api gen   -config <provider>.yaml [-o <provider>-v1.json]
//	tofu-api version
//
// The service keeps no state: the caller keeps the id a create returns.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/infrapi/lib/pkg/auth"
	"github.com/infrapi/lib/pkg/config"
	"github.com/infrapi/lib/pkg/listener"
	"github.com/infrapi/lib/pkg/logger"
	"github.com/infrapi/lib/pkg/server"
	"github.com/infrapi/lib/pkg/tofu"
)

const usage = `tofu-api serves resource types of a Terraform provider as a REST API,
with the OpenAPI contract generated from their schemas.

Usage:
  tofu-api serve -config <provider>.yaml
  tofu-api gen   -config <provider>.yaml [-o <file>]
  tofu-api version

Commands:
  serve    serve the API of the configuration's provider
  gen      check the configuration against the provider's schemas, and write
           the contract the service would serve
  version  print the version

The configuration names the provider, by source and version or by
binary_path, the resource types to expose and the rules over them (see the
README). A provider named by source is installed from the network mirror of
Terraform's CLI configuration, TF_CLI_CONFIG_FILE. serve reads its settings
from the environment, INFRAPI_*.

Run 'tofu-api <command> -h' for its flags.
`

func main() {
	err := run(os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	case errors.Is(err, errShown):
		os.Exit(2)
	case errors.As(err, new(usageError)):
		fmt.Fprintln(os.Stderr, "tofu-api:", err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "tofu-api:", err)
		os.Exit(1)
	}
}

// usageError is a command line tofu-api does not understand: exit 2.
type usageError string

func (e usageError) Error() string { return string(e) }

// errShown is a usage error whose message and usage were already written.
var errShown = errors.New("shown")

func run(args []string, stdout, stderr io.Writer) (err error) {
	if len(args) == 0 {
		if _, err := fmt.Fprint(stderr, usage); err != nil {
			return err
		}
		return errShown
	}
	cmd := args[0]
	switch cmd {
	case "-h", "-help", "--help", "help":
		_, err := fmt.Fprint(stdout, usage)
		return err
	case "version", "-version", "--version":
		_, err := fmt.Fprintln(stdout, "tofu-api", version())
		return err
	case "gen", "serve":
	default:
		return usageError(fmt.Sprintf("unknown command %q, run 'tofu-api -h' for the usage", cmd))
	}

	fs := flag.NewFlagSet("tofu-api "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "the provider's configuration `file` (required)")
	out := new(string)
	if cmd == "gen" {
		out = fs.String("o", "-", "where to write the contract: a `file`, or - for stdout")
	}
	header := "Usage: tofu-api serve -config <provider>.yaml\n\n" +
		"Serves the API of the configuration's provider; settings from INFRAPI_*.\n\n"
	if cmd == "gen" {
		header = "Usage: tofu-api gen -config <provider>.yaml [-o <file>]\n\n" +
			"Checks the configuration as serve does at start, against the provider's\n" +
			"schemas, without its credentials, and writes the OpenAPI contract.\n\n"
	}
	// The flag package calls Usage without a way to fail: a failed write is
	// returned once Parse is done.
	var usageErr error
	fs.Usage = func() {
		if _, err := fmt.Fprint(fs.Output(), header); err != nil {
			usageErr = err
			return
		}
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		switch {
		case usageErr != nil:
			return usageErr
		case errors.Is(err, flag.ErrHelp):
			return err
		}
		return errShown // the flag package wrote the error and the usage
	}
	switch {
	case fs.NArg() > 0:
		return usageError(fmt.Sprintf("%s: unexpected argument %q, run 'tofu-api %[1]s -h' for the usage", cmd, fs.Arg(0)))
	case *path == "":
		return usageError(fmt.Sprintf("%s: -config is required, run 'tofu-api %[1]s -h' for the usage", cmd))
	}
	f, err := readConfig(*path)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if f.Provider.Source != "" {
		if f.Provider.BinaryPath, err = installProvider(ctx, f.Provider); err != nil {
			return err
		}
	}

	a, contract, err := generate(ctx, f, *path)
	if err != nil {
		return err
	}
	if cmd == "gen" {
		if *out == "-" {
			_, err := stdout.Write(contract)
			return err
		}
		return os.WriteFile(*out, contract, 0o600) // #nosec G703 -- path given on the command line by the operator
	}
	return serve(ctx, f, a, contract)
}

// version is the module's version the binary was built from, as go build
// records it: the tag, or (devel).
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	return info.Main.Version
}

// generate reads the schemas from a provider started for that alone, with
// no configuration nor credentials, and generates the contract.
func generate(ctx context.Context, f configFile, filename string) (a *api, contract []byte, err error) {
	p, err := tofu.Start(ctx, f.Provider.Name, f.Provider.BinaryPath)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, p.Close()) }()
	return build(f, filename, p)
}

// build reads the schemas from the provider and generates the contract,
// which kin-openapi (through the server) will load and validate.
func build(f configFile, filename string, p schemaSource) (*api, []byte, error) {
	schemas, err := loadSchemas(p, f.typeNames())
	if err != nil {
		return nil, nil, err
	}
	a := &api{provider: f.Provider.Name, basePath: f.API.BasePath, namespacePattern: f.API.NamespacePattern, types: schemas, bearer: f.Auth != nil}
	if a.rules, a.errors, err = parseRules(f, filename, schemas); err != nil {
		return nil, nil, err
	}
	contract, err := json.MarshalIndent(a.document(), "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("encode contract: %w", err)
	}
	return a, append(contract, '\n'), nil
}

func serve(ctx context.Context, f configFile, a *api, contract []byte) (err error) {
	vars, err := config.NewConfig()
	if err != nil {
		return err
	}
	cfg, err := vars.GetAppConfig()
	if err != nil {
		return err
	}
	log, err := logger.New(logger.Options{Level: cfg.AppLogLevel.String(), Format: cfg.AppLogFormat})
	if err != nil {
		return err
	}
	logger.SetDefault(log)

	ps, closeProviders, err := startProviders(ctx, f)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeProviders()) }()

	opts := []server.Option{server.WithOpenAPI(server.OpenAPISpec{
		Name: f.Provider.Name + " v1", Path: f.Provider.Name + "/v1.json", Content: contract,
	})}
	pl, isPool := ps.(*pool)
	if isPool {
		opts = append(opts, server.WithOperations(providersOperation))
	}
	var rbac *access
	if f.Auth != nil {
		client, err := auth.NewClient(auth.ClientOptions{BaseURL: f.Auth.URL, CacheTTL: f.Auth.CacheTTL})
		if err != nil {
			return err
		}
		// Every route added after the engine's own /-/ endpoints needs a
		// token the service resolves.
		opts = append(opts, server.WithAuth(client.Middleware()))
		if *f.Auth.Authorize {
			checker := client
			if f.Auth.AuthorizeURL != f.Auth.URL {
				// The RBAC on a service of its own; its decisions are
				// never cached, whichever client asks.
				if checker, err = auth.NewClient(auth.ClientOptions{BaseURL: f.Auth.AuthorizeURL}); err != nil {
					return err
				}
			}
			rbac = &access{checker: checker, api: f.Auth.API}
		}
	}
	engine, err := server.NewServerGin(cfg, opts...)
	if err != nil {
		return err
	}
	h := newHandlers(a, ps, f.Provider.Timeout)
	h.access = rbac
	h.routes(engine)
	if isPool {
		pl.routes(engine)
	}

	socket, err := listener.Listen(cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: engine, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() {
		log.Info("listening", logger.String("addr", socket.Addr().String()))
		served <- srv.Serve(socket)
	}()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// startProviders starts the provider that serves every namespace, or, with
// namespaces configured, the pool that starts one per namespace on demand.
func startProviders(ctx context.Context, f configFile) (providers, func() error, error) {
	base, err := f.Provider.resolved(os.LookupEnv)
	if err != nil {
		return nil, nil, err
	}
	if f.Namespaces == nil {
		p, err := tofu.Start(ctx, f.Provider.Name, f.Provider.BinaryPath)
		if err != nil {
			return nil, nil, err
		}
		if err := p.Configure(ctx, "", base); err != nil {
			return nil, nil, errors.Join(err, p.Close())
		}
		return newShared(p, f.Provider.Parallelism), p.Close, nil
	}

	vc, err := f.Namespaces.Vault.resolved(os.LookupEnv)
	if err != nil {
		return nil, nil, err
	}
	v, err := newNamespaceSecrets(ctx, vc, 0)
	if err != nil {
		return nil, nil, err
	}
	pl := newPool(func(ctx context.Context, namespace string) (process, error) {
		secret, err := v.namespaceConfig(ctx, namespace)
		if err != nil {
			return nil, err
		}
		p, err := tofu.Start(ctx, f.Provider.Name, f.Provider.BinaryPath)
		if err != nil {
			return nil, err
		}
		config := maps.Clone(base)
		maps.Copy(config, secret)
		if err := p.Configure(ctx, "", config); err != nil {
			return nil, errors.Join(err, p.Close())
		}
		return p, nil
	}, *f.Namespaces, f.Provider.Parallelism)
	go pl.sweepEvery(ctx, min(f.Namespaces.Idle, f.Namespaces.MaxAge)/4)
	return pl, pl.Close, nil
}
