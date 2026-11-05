// Command tofu-api exposes resource types of an OpenTofu-protocol
// provider as an HTTP API, without writing code per resource: it reads each
// type's schema from the provider, generates the OpenAPI contract from it,
// and serves create/read/update/delete through pkg/tofu. One configuration
// file per provider says which, and how (see configFile).
//
//	tofu-api gen   -config <provider>.yaml -o <provider>-v1.json
//	tofu-api serve -config <provider>.yaml
//
// The service keeps no state: the caller keeps the id a create returns.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/infrapi/lib/pkg/config"
	"github.com/infrapi/lib/pkg/listener"
	"github.com/infrapi/lib/pkg/logger"
	"github.com/infrapi/lib/pkg/server"
	"github.com/infrapi/lib/pkg/tofu"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tofu-api:", err)
		os.Exit(1)
	}
}

func run(args []string) (err error) {
	if len(args) == 0 || (args[0] != "gen" && args[0] != "serve") {
		return errors.New("usage: tofu-api gen|serve -config <provider>.yaml")
	}
	cmd := args[0]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	path := fs.String("config", "", "YAML file of the provider to run, the resource types to expose and the rules over them")
	out := fs.String("o", "-", "gen: output file, - for stdout")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("-config is required")
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
			_, err := os.Stdout.Write(contract)
			return err
		}
		return os.WriteFile(*out, contract, 0o600) // #nosec G703 -- path given on the command line by the operator
	}
	return serve(ctx, f, a, contract)
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
	a := &api{provider: f.Provider.Name, basePath: f.API.BasePath, namespacePattern: f.API.NamespacePattern, types: schemas}
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
	engine, err := server.NewServerGin(cfg, opts...)
	if err != nil {
		return err
	}
	h := newHandlers(a, ps, f.Provider.Timeout)
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
